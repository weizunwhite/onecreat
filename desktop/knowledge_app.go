package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	fileenc "reasonix/internal/fileutil/encoding"
)

const (
	knowledgeStoreVersion = 1
	knowledgeMaxFileBytes = 8 * 1024 * 1024
	knowledgeChunkRunes   = 2200
	knowledgeChunkOverlap = 250
)

var (
	knowledgeMu sync.Mutex

	knowledgeSupportedExt = map[string]bool{
		".c": true, ".cc": true, ".cpp": true, ".cxx": true, ".h": true, ".hpp": true,
		".css": true, ".csv": true, ".go": true, ".html": true, ".ino": true,
		".java": true, ".js": true, ".jsx": true, ".json": true, ".kt": true,
		".lua": true, ".md": true, ".mdx": true, ".mjs": true, ".py": true,
		".rs": true, ".sh": true, ".sql": true, ".swift": true, ".toml": true,
		".ts": true, ".tsx": true, ".txt": true, ".yaml": true, ".yml": true,
	}
)

// KnowledgeBaseView is one local knowledge collection shown in the drawer.
type KnowledgeBaseView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
	Documents int    `json:"documents"`
	Chunks    int    `json:"chunks"`
}

// KnowledgeDocumentView is one imported local file.
type KnowledgeDocumentView struct {
	ID           string `json:"id"`
	BaseID       string `json:"baseId"`
	Name         string `json:"name"`
	OriginalPath string `json:"originalPath"`
	StoredPath   string `json:"storedPath,omitempty"`
	Size         int64  `json:"size"`
	ImportedAt   int64  `json:"importedAt"`
	Status       string `json:"status"`
	Chunks       int    `json:"chunks"`
	Error        string `json:"error,omitempty"`
}

// KnowledgeView is the full local knowledge-base snapshot.
type KnowledgeView struct {
	StoreDir            string                  `json:"storeDir"`
	Mode                string                  `json:"mode"`
	SupportedExtensions []string                `json:"supportedExtensions"`
	Bases               []KnowledgeBaseView     `json:"bases"`
	Documents           []KnowledgeDocumentView `json:"documents"`
}

type KnowledgeImportIssue struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

type KnowledgeImportResult struct {
	Imported []KnowledgeDocumentView `json:"imported"`
	Skipped  []KnowledgeImportIssue  `json:"skipped"`
}

type KnowledgeSearchResult struct {
	Query   string               `json:"query"`
	Matches []KnowledgeMatchView `json:"matches"`
}

type KnowledgePromptView struct {
	Prompt  string               `json:"prompt"`
	Sources []KnowledgeMatchView `json:"sources"`
}

type KnowledgeMatchView struct {
	BaseID       string  `json:"baseId"`
	BaseName     string  `json:"baseName"`
	DocumentID   string  `json:"documentId"`
	DocumentName string  `json:"documentName"`
	ChunkID      string  `json:"chunkId"`
	ChunkIndex   int     `json:"chunkIndex"`
	Text         string  `json:"text"`
	Score        float64 `json:"score"`
}

type knowledgeStore struct {
	Version   int                       `json:"version"`
	Bases     []knowledgeBaseRecord     `json:"bases"`
	Documents []knowledgeDocumentRecord `json:"documents"`
}

type knowledgeBaseRecord struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
}

type knowledgeDocumentRecord struct {
	ID           string                 `json:"id"`
	BaseID       string                 `json:"baseId"`
	Name         string                 `json:"name"`
	OriginalPath string                 `json:"originalPath"`
	StoredPath   string                 `json:"storedPath,omitempty"`
	Size         int64                  `json:"size"`
	ImportedAt   int64                  `json:"importedAt"`
	Status       string                 `json:"status"`
	Error        string                 `json:"error,omitempty"`
	Chunks       []knowledgeChunkRecord `json:"chunks"`
}

type knowledgeChunkRecord struct {
	ID    string `json:"id"`
	Index int    `json:"index"`
	Text  string `json:"text"`
}

// KnowledgeView returns the local Mode A knowledge-base snapshot. It does not
// read project memory or any workspace files automatically; only explicitly
// imported files appear here.
func (a *App) KnowledgeView() (KnowledgeView, error) {
	knowledgeMu.Lock()
	defer knowledgeMu.Unlock()

	root, st, err := knowledgeLoadStore()
	if err != nil {
		return KnowledgeView{}, err
	}
	return knowledgeStoreView(root, st), nil
}

func (a *App) KnowledgeCreate(name string) (KnowledgeBaseView, error) {
	knowledgeMu.Lock()
	defer knowledgeMu.Unlock()

	root, st, err := knowledgeLoadStore()
	if err != nil {
		return KnowledgeBaseView{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "未命名知识库"
	}
	now := time.Now().UnixMilli()
	base := knowledgeBaseRecord{
		ID:        knowledgeID("kb"),
		Name:      name,
		CreatedAt: now,
		UpdatedAt: now,
	}
	st.Bases = append(st.Bases, base)
	if err := knowledgeSaveStore(root, st); err != nil {
		return KnowledgeBaseView{}, err
	}
	return knowledgeBaseView(base, st.Documents), nil
}

func (a *App) KnowledgeDelete(id string) error {
	knowledgeMu.Lock()
	defer knowledgeMu.Unlock()

	root, st, err := knowledgeLoadStore()
	if err != nil {
		return err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("knowledge base id is required")
	}
	found := false
	bases := st.Bases[:0]
	for _, base := range st.Bases {
		if base.ID == id {
			found = true
			continue
		}
		bases = append(bases, base)
	}
	if !found {
		return fmt.Errorf("knowledge base not found: %s", id)
	}
	docs := st.Documents[:0]
	for _, doc := range st.Documents {
		if doc.BaseID != id {
			docs = append(docs, doc)
		}
	}
	st.Bases = bases
	st.Documents = docs
	_ = os.RemoveAll(filepath.Join(root, "files", id))
	return knowledgeSaveStore(root, st)
}

// KnowledgeImportFiles opens a native file picker and imports selected local
// text/code files into one knowledge base.
func (a *App) KnowledgeImportFiles(baseID string) (KnowledgeImportResult, error) {
	if a.ctx == nil {
		return KnowledgeImportResult{Imported: []KnowledgeDocumentView{}, Skipped: []KnowledgeImportIssue{}}, nil
	}
	paths, err := a.sh().OpenMultipleFilesDialog(DialogOptions{
		Title:            "导入本地知识库文件",
		DefaultDirectory: a.workspaceRoot(),
		Filters: []FileFilter{
			{
				DisplayName: "Text, Markdown, Code",
				Pattern:     "*.txt;*.md;*.mdx;*.csv;*.json;*.yaml;*.yml;*.toml;*.go;*.py;*.js;*.ts;*.tsx;*.jsx;*.ino;*.cpp;*.c;*.h;*.hpp;*.html;*.css;*.rs;*.java;*.swift;*.sh;*.sql",
			},
			{DisplayName: "All Files", Pattern: "*.*"},
		},
	})
	if err != nil || len(paths) == 0 {
		return KnowledgeImportResult{Imported: []KnowledgeDocumentView{}, Skipped: []KnowledgeImportIssue{}}, err
	}
	return a.knowledgeImportPaths(baseID, paths)
}

// KnowledgeImportPaths 按绝对路径导入知识库文件(不弹原生对话框)。Web 模式下前端先把
// 浏览器选的文件 POST /upload 落到临时目录,拿到路径后调这里;桌面版仍走 KnowledgeImportFiles。
func (a *App) KnowledgeImportPaths(baseID string, paths []string) (KnowledgeImportResult, error) {
	return a.knowledgeImportPaths(baseID, paths)
}

func (a *App) knowledgeImportPaths(baseID string, paths []string) (KnowledgeImportResult, error) {
	knowledgeMu.Lock()
	defer knowledgeMu.Unlock()

	root, st, err := knowledgeLoadStore()
	if err != nil {
		return KnowledgeImportResult{}, err
	}
	baseID = strings.TrimSpace(baseID)
	if !knowledgeHasBase(st, baseID) {
		return KnowledgeImportResult{}, fmt.Errorf("knowledge base not found: %s", baseID)
	}

	result := KnowledgeImportResult{Imported: []KnowledgeDocumentView{}, Skipped: []KnowledgeImportIssue{}}
	for _, path := range paths {
		doc, issue := knowledgeImportOne(root, &st, baseID, path)
		if issue.Error != "" {
			result.Skipped = append(result.Skipped, issue)
			continue
		}
		result.Imported = append(result.Imported, knowledgeDocumentView(doc))
	}
	now := time.Now().UnixMilli()
	for i := range st.Bases {
		if st.Bases[i].ID == baseID && len(result.Imported) > 0 {
			st.Bases[i].UpdatedAt = now
		}
	}
	if err := knowledgeSaveStore(root, st); err != nil {
		return KnowledgeImportResult{}, err
	}
	return result, nil
}

func (a *App) KnowledgeSearch(baseIDs []string, query string, limit int) (KnowledgeSearchResult, error) {
	knowledgeMu.Lock()
	defer knowledgeMu.Unlock()

	_, st, err := knowledgeLoadStore()
	if err != nil {
		return KnowledgeSearchResult{}, err
	}
	matches := knowledgeSearch(st, baseIDs, query, limit)
	return KnowledgeSearchResult{Query: strings.TrimSpace(query), Matches: matches}, nil
}

// KnowledgeBuildPrompt creates the augmented user prompt for Mode A. Documents
// stay local; only the top matching snippets are included in this one model call.
func (a *App) KnowledgeBuildPrompt(baseIDs []string, question string, limit int) (KnowledgePromptView, error) {
	knowledgeMu.Lock()
	defer knowledgeMu.Unlock()

	_, st, err := knowledgeLoadStore()
	if err != nil {
		return KnowledgePromptView{}, err
	}
	question = strings.TrimSpace(question)
	matches := knowledgeSearch(st, baseIDs, question, limit)
	if question == "" || len(matches) == 0 {
		return KnowledgePromptView{Prompt: question, Sources: matches}, nil
	}

	var b strings.Builder
	b.WriteString("你正在回答用户问题。用户问题和当前对话上下文优先；下面的本地知识库片段只是候选参考，可能与当前问题无关。\n")
	b.WriteString("隐私与事实规则：这些片段只来自客户本机知识库；只在片段明显相关时使用；资料不相关时忽略；不得把片段中的任务类型、项目名称或提交要求强行套到用户问题上；引用资料事实时标注来源编号，如 [1]。\n\n")
	b.WriteString("# 用户问题\n")
	b.WriteString(question)
	b.WriteString("\n\n# 本地知识库片段\n")
	for i, m := range matches {
		fmt.Fprintf(&b, "[%d] 知识库：%s / 文件：%s / 片段：%d\n", i+1, m.BaseName, m.DocumentName, m.ChunkIndex+1)
		b.WriteString(strings.TrimSpace(m.Text))
		b.WriteString("\n\n")
	}
	return KnowledgePromptView{Prompt: b.String(), Sources: matches}, nil
}

func knowledgeRootDir() (string, error) {
	if override := strings.TrimSpace(os.Getenv("ONECREAT_KNOWLEDGE_DIR")); override != "" {
		return filepath.Abs(override)
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "onecreat", "knowledge"), nil
}

func knowledgeLoadStore() (string, knowledgeStore, error) {
	root, err := knowledgeRootDir()
	if err != nil {
		return "", knowledgeStore{}, err
	}
	if err := os.MkdirAll(filepath.Join(root, "files"), 0o755); err != nil {
		return "", knowledgeStore{}, err
	}
	path := filepath.Join(root, "index.json")
	st := knowledgeStore{Version: knowledgeStoreVersion, Bases: []knowledgeBaseRecord{}, Documents: []knowledgeDocumentRecord{}}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return root, st, nil
	}
	if err != nil {
		return "", knowledgeStore{}, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return "", knowledgeStore{}, fmt.Errorf("read knowledge index: %w", err)
	}
	if st.Bases == nil {
		st.Bases = []knowledgeBaseRecord{}
	}
	if st.Documents == nil {
		st.Documents = []knowledgeDocumentRecord{}
	}
	if st.Version == 0 {
		st.Version = knowledgeStoreVersion
	}
	return root, st, nil
}

func knowledgeSaveStore(root string, st knowledgeStore) error {
	st.Version = knowledgeStoreVersion
	if st.Bases == nil {
		st.Bases = []knowledgeBaseRecord{}
	}
	if st.Documents == nil {
		st.Documents = []knowledgeDocumentRecord{}
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, "index.json"), b, 0o600)
}

func knowledgeStoreView(root string, st knowledgeStore) KnowledgeView {
	bases := make([]KnowledgeBaseView, 0, len(st.Bases))
	for _, base := range st.Bases {
		bases = append(bases, knowledgeBaseView(base, st.Documents))
	}
	sort.Slice(bases, func(i, j int) bool { return bases[i].UpdatedAt > bases[j].UpdatedAt })

	docs := make([]KnowledgeDocumentView, 0, len(st.Documents))
	for _, doc := range st.Documents {
		docs = append(docs, knowledgeDocumentView(doc))
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].ImportedAt > docs[j].ImportedAt })

	exts := make([]string, 0, len(knowledgeSupportedExt))
	for ext := range knowledgeSupportedExt {
		exts = append(exts, ext)
	}
	sort.Strings(exts)

	return KnowledgeView{
		StoreDir:            root,
		Mode:                "模式 A：本地存储 + 客户自填 API；当前检索在本机完成，只把命中片段加入本次请求。",
		SupportedExtensions: exts,
		Bases:               bases,
		Documents:           docs,
	}
}

func knowledgeBaseView(base knowledgeBaseRecord, docs []knowledgeDocumentRecord) KnowledgeBaseView {
	view := KnowledgeBaseView{
		ID:        base.ID,
		Name:      base.Name,
		CreatedAt: base.CreatedAt,
		UpdatedAt: base.UpdatedAt,
	}
	for _, doc := range docs {
		if doc.BaseID != base.ID || doc.Status != "ready" {
			continue
		}
		view.Documents++
		view.Chunks += len(doc.Chunks)
	}
	return view
}

func knowledgeDocumentView(doc knowledgeDocumentRecord) KnowledgeDocumentView {
	return KnowledgeDocumentView{
		ID:           doc.ID,
		BaseID:       doc.BaseID,
		Name:         doc.Name,
		OriginalPath: doc.OriginalPath,
		StoredPath:   doc.StoredPath,
		Size:         doc.Size,
		ImportedAt:   doc.ImportedAt,
		Status:       doc.Status,
		Chunks:       len(doc.Chunks),
		Error:        doc.Error,
	}
}

func knowledgeHasBase(st knowledgeStore, baseID string) bool {
	for _, base := range st.Bases {
		if base.ID == baseID {
			return true
		}
	}
	return false
}

func knowledgeImportOne(root string, st *knowledgeStore, baseID, path string) (knowledgeDocumentRecord, KnowledgeImportIssue) {
	path = strings.TrimSpace(path)
	if path == "" {
		return knowledgeDocumentRecord{}, KnowledgeImportIssue{Error: "文件路径为空"}
	}
	abs, err := filepath.Abs(path)
	if err == nil {
		path = abs
	}
	info, err := os.Stat(path)
	if err != nil {
		return knowledgeDocumentRecord{}, KnowledgeImportIssue{Path: path, Error: err.Error()}
	}
	if info.IsDir() {
		return knowledgeDocumentRecord{}, KnowledgeImportIssue{Path: path, Error: "暂不支持导入文件夹，请选择具体文件"}
	}
	if !info.Mode().IsRegular() {
		return knowledgeDocumentRecord{}, KnowledgeImportIssue{Path: path, Error: "不是普通文件"}
	}
	if info.Size() > knowledgeMaxFileBytes {
		return knowledgeDocumentRecord{}, KnowledgeImportIssue{Path: path, Error: "文件超过 8MB，第一版请拆分后导入"}
	}
	ext := strings.ToLower(filepath.Ext(path))
	if !knowledgeSupportedExt[ext] {
		return knowledgeDocumentRecord{}, KnowledgeImportIssue{Path: path, Error: "第一版先支持文本、Markdown 和代码文件；PDF/Word/PPT/Excel 后续再接入解析"}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return knowledgeDocumentRecord{}, KnowledgeImportIssue{Path: path, Error: err.Error()}
	}
	enc, data := fileenc.Detect(raw)
	text := string(fileenc.Decode(data, enc))
	if strings.TrimSpace(text) == "" {
		return knowledgeDocumentRecord{}, KnowledgeImportIssue{Path: path, Error: "文件没有可检索文本"}
	}
	if strings.ContainsRune(text, '\x00') {
		return knowledgeDocumentRecord{}, KnowledgeImportIssue{Path: path, Error: "文件看起来是二进制内容"}
	}

	for _, doc := range st.Documents {
		if doc.BaseID == baseID && doc.OriginalPath == path && doc.StoredPath != "" {
			_ = os.Remove(doc.StoredPath)
		}
	}
	filtered := st.Documents[:0]
	for _, doc := range st.Documents {
		if !(doc.BaseID == baseID && doc.OriginalPath == path) {
			filtered = append(filtered, doc)
		}
	}
	st.Documents = filtered

	docID := knowledgeID("doc")
	storedDir := filepath.Join(root, "files", baseID)
	if err := os.MkdirAll(storedDir, 0o755); err != nil {
		return knowledgeDocumentRecord{}, KnowledgeImportIssue{Path: path, Error: err.Error()}
	}
	storedPath := filepath.Join(storedDir, docID+ext)
	if err := copyFile(path, storedPath); err != nil {
		return knowledgeDocumentRecord{}, KnowledgeImportIssue{Path: path, Error: err.Error()}
	}
	doc := knowledgeDocumentRecord{
		ID:           docID,
		BaseID:       baseID,
		Name:         filepath.Base(path),
		OriginalPath: path,
		StoredPath:   storedPath,
		Size:         info.Size(),
		ImportedAt:   time.Now().UnixMilli(),
		Status:       "ready",
		Chunks:       knowledgeChunkText(docID, text),
	}
	st.Documents = append(st.Documents, doc)
	return doc, KnowledgeImportIssue{}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func knowledgeChunkText(docID, text string) []knowledgeChunkRecord {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) == 0 {
		return []knowledgeChunkRecord{}
	}
	chunks := []knowledgeChunkRecord{}
	for start, index := 0, 0; start < len(runes); index++ {
		end := start + knowledgeChunkRunes
		if end > len(runes) {
			end = len(runes)
		}
		part := strings.TrimSpace(string(runes[start:end]))
		if part != "" {
			chunks = append(chunks, knowledgeChunkRecord{
				ID:    fmt.Sprintf("%s:%d", docID, index),
				Index: index,
				Text:  part,
			})
		}
		if end == len(runes) {
			break
		}
		next := end - knowledgeChunkOverlap
		if next <= start {
			next = end
		}
		start = next
	}
	return chunks
}

func knowledgeSearch(st knowledgeStore, baseIDs []string, query string, limit int) []KnowledgeMatchView {
	query = strings.TrimSpace(query)
	if query == "" {
		return []KnowledgeMatchView{}
	}
	if limit <= 0 || limit > 20 {
		limit = 8
	}
	allowed := map[string]bool{}
	for _, id := range baseIDs {
		id = strings.TrimSpace(id)
		if id != "" {
			allowed[id] = true
		}
	}
	baseNames := map[string]string{}
	for _, base := range st.Bases {
		baseNames[base.ID] = base.Name
	}

	type scored struct {
		match KnowledgeMatchView
	}
	var scoredMatches []scored
	for _, doc := range st.Documents {
		if doc.Status != "ready" {
			continue
		}
		if len(allowed) > 0 && !allowed[doc.BaseID] {
			continue
		}
		for _, chunk := range doc.Chunks {
			score := knowledgeScore(query, chunk.Text, doc.Name)
			if score <= 0 {
				continue
			}
			scoredMatches = append(scoredMatches, scored{
				match: KnowledgeMatchView{
					BaseID:       doc.BaseID,
					BaseName:     baseNames[doc.BaseID],
					DocumentID:   doc.ID,
					DocumentName: doc.Name,
					ChunkID:      chunk.ID,
					ChunkIndex:   chunk.Index,
					Text:         knowledgeExcerpt(chunk.Text, query, 1200),
					Score:        score,
				},
			})
		}
	}
	sort.Slice(scoredMatches, func(i, j int) bool {
		if scoredMatches[i].match.Score == scoredMatches[j].match.Score {
			return scoredMatches[i].match.DocumentName < scoredMatches[j].match.DocumentName
		}
		return scoredMatches[i].match.Score > scoredMatches[j].match.Score
	})
	if len(scoredMatches) > limit {
		scoredMatches = scoredMatches[:limit]
	}
	out := make([]KnowledgeMatchView, 0, len(scoredMatches))
	for _, item := range scoredMatches {
		out = append(out, item.match)
	}
	return out
}

func knowledgeScore(query, text, name string) float64 {
	q := strings.ToLower(query)
	t := strings.ToLower(text)
	n := strings.ToLower(name)
	score := 0.0
	if strings.Contains(t, q) {
		score += 8
	}
	if strings.Contains(n, q) {
		score += 5
	}
	for _, token := range knowledgeTokens(query) {
		if utf8.RuneCountInString(token) < 2 {
			continue
		}
		count := strings.Count(t, token)
		if count > 0 {
			score += float64(count) * (1 + float64(utf8.RuneCountInString(token))/5)
		}
		if strings.Contains(n, token) {
			score += 2
		}
	}
	return score
}

// knowledgeTokens 把查询切成检索用的 token。
// 关键:中文汉字在 Unicode 里也算「字母」,如果只按非字母切，一整句中文
// 会变成一个超长 token，strings.Count 几乎不可能命中——中文需求必漏召回。
// 这里区别对待:
//   - 英文/数字(型号如 esp32、sr04):整词保留;
//   - 中文:按「相邻二字词」(bigram)切，既能召回又比单字更精准
//     (评分循环会跳过长度<2 的 token，所以单字不单独成 token，
//     单字查询交给整句 strings.Contains 兜底)。
func knowledgeTokens(s string) []string {
	s = strings.ToLower(s)
	out := make([]string, 0, 16)
	seen := map[string]bool{}
	add := func(tok string) {
		if tok == "" || seen[tok] {
			return
		}
		seen[tok] = true
		out = append(out, tok)
	}
	runes := []rune(s)
	ascii := make([]rune, 0, 8) // 累积连续的英文/数字，作为整词
	flushASCII := func() {
		if len(ascii) > 0 {
			add(string(ascii))
			ascii = ascii[:0]
		}
	}
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case isCJK(r):
			flushASCII()
			if i+1 < len(runes) && isCJK(runes[i+1]) {
				add(string(runes[i : i+2])) // 相邻两个汉字组成二字词
			}
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			ascii = append(ascii, r)
		default:
			flushASCII()
		}
	}
	flushASCII()
	return out
}

// isCJK 判断是否为中日韩统一表意文字(主要覆盖中文汉字)。
func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r)
}

func knowledgeExcerpt(text, query string, maxRunes int) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) <= maxRunes {
		return string(runes)
	}
	lowerRunes := []rune(strings.ToLower(string(runes)))
	needle := []rune(strings.ToLower(strings.TrimSpace(query)))
	pos := -1
	if len(needle) > 0 && len(needle) < len(lowerRunes) {
	outer:
		for i := 0; i <= len(lowerRunes)-len(needle); i++ {
			for j := range needle {
				if lowerRunes[i+j] != needle[j] {
					continue outer
				}
			}
			pos = i
			break
		}
	}
	if pos < 0 {
		for _, token := range knowledgeTokens(query) {
			pos = strings.Index(strings.ToLower(string(runes)), token)
			if pos >= 0 {
				pos = utf8.RuneCountInString(string([]byte(string(runes))[:pos]))
				break
			}
		}
	}
	if pos < 0 {
		return string(runes[:maxRunes]) + "..."
	}
	start := pos - maxRunes/3
	if start < 0 {
		start = 0
	}
	end := start + maxRunes
	if end > len(runes) {
		end = len(runes)
		start = end - maxRunes
		if start < 0 {
			start = 0
		}
	}
	prefix := ""
	if start > 0 {
		prefix = "..."
	}
	suffix := ""
	if end < len(runes) {
		suffix = "..."
	}
	return prefix + string(runes[start:end]) + suffix
}

func knowledgeID(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}
