package workflow

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestJournalAppendAndReplay：追加的事件按写入顺序原样回放。
func TestJournalAppendAndReplay(t *testing.T) {
	dir := t.TempDir()
	ts := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	want := []Event{
		{TS: ts, Kind: KindMaterialStarted, Material: "技术方案"},
		{TS: ts.Add(time.Minute), Kind: KindQualityNote, Material: "技术方案", Reason: "看图质检跳过"},
		{TS: ts.Add(2 * time.Minute), Kind: KindMaterialSucceeded, Material: "技术方案",
			Outputs: []string{"02_平台材料/技术方案_飞鸟志.md"}, Retries: 1},
	}
	for _, ev := range want {
		if err := Append(dir, ev); err != nil {
			t.Fatal(err)
		}
	}
	got, skipped, err := ReplaySkipped(dir)
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 0 {
		t.Errorf("不该有坏行，实际跳了 %d", skipped)
	}
	if len(got) != len(want) {
		t.Fatalf("回放条数不对：%d != %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Kind != want[i].Kind || got[i].Material != want[i].Material ||
			got[i].Reason != want[i].Reason || got[i].Retries != want[i].Retries ||
			!got[i].TS.Equal(want[i].TS) {
			t.Errorf("第 %d 条不一致：\n got=%+v\nwant=%+v", i, got[i], want[i])
		}
	}
	if len(got[2].Outputs) != 1 || got[2].Outputs[0] != "02_平台材料/技术方案_飞鸟志.md" {
		t.Errorf("Outputs 没回放出来：%+v", got[2])
	}
}

// TestJournalSkipsBadLines：坏行跳过并计数，不 panic，也不让整份台账作废。
func TestJournalSkipsBadLines(t *testing.T) {
	dir := t.TempDir()
	if err := Append(dir, Event{Kind: KindMaterialStarted, Material: "教案"}); err != nil {
		t.Fatal(err)
	}
	// 手工往台账里塞三种坏行：半行 JSON、纯垃圾、合法 JSON 但没有 kind。
	f, err := os.OpenFile(JournalPath(dir), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{\"ts\":\"2026-08-2\n乱码乱码\n{\"material\":\"教案\"}\n\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := Append(dir, Event{Kind: KindMaterialSucceeded, Material: "教案"}); err != nil {
		t.Fatal(err)
	}

	got, skipped, err := ReplaySkipped(dir)
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 3 {
		t.Errorf("应跳过 3 条坏行，实际 %d", skipped)
	}
	if len(got) != 2 || got[0].Kind != KindMaterialStarted || got[1].Kind != KindMaterialSucceeded {
		t.Errorf("好行要全部保住：%+v", got)
	}
}

// TestJournalReplayMissingFileIsNotAnError：没跑过作业的项目，回放出空清单而不是错误。
func TestJournalReplayMissingFileIsNotAnError(t *testing.T) {
	got, err := Replay(t.TempDir())
	if err != nil {
		t.Fatalf("台账不存在不该报错：%v", err)
	}
	if len(got) != 0 {
		t.Errorf("应是空清单，实际 %+v", got)
	}
}

// TestJournalRejectsUnknownKind：拼错的 kind 当场报错，不许静默写进台账。
func TestJournalRejectsUnknownKind(t *testing.T) {
	dir := t.TempDir()
	if err := Append(dir, Event{Kind: "material_done"}); err == nil {
		t.Fatal("非法 kind 应报错")
	}
	if _, err := os.Stat(JournalPath(dir)); !os.IsNotExist(err) {
		t.Errorf("拒绝的事件不该建出台账文件：%v", err)
	}
}

// TestJournalLivesUnderDotOnecreat：落点固定在 <项目>/.onecreat/workflow.jsonl。
// 这个路径是 dot 路径，project_sync.py 整树排除它——本地不镜像是拍板结论
// （05_夹具与基线 §6 第 2 条方案 a），换路径等于推翻它。
func TestJournalLivesUnderDotOnecreat(t *testing.T) {
	dir := t.TempDir()
	if err := Append(dir, Event{Kind: KindMaterialStarted, Material: "教案"}); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, ".onecreat", "workflow.jsonl")
	if JournalPath(dir) != want {
		t.Errorf("台账路径不对：%s != %s", JournalPath(dir), want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("台账没落在 .onecreat/ 下：%v", err)
	}
}
