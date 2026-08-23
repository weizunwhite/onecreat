// Package numbering 负责 P26 项目编号(三端身份证)的预告与占号。
//
// 迁移期口径(01_M0决策记录 §1.1 拍板 1 = A):**发号权威在 NAS**,OneCreat 经
// NAS 的只读预告接口拿号;M5 冻结日一次性把权威切到本地文件(`Local`)。
// 两个实现**不得同时启用**(§1.1「全期」约束:不做双写,配置只能选一个)。
package numbering

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const (
	// CodePattern 是**新发号**的严口径:业务线字母必填。
	CodePattern = `^P26[CBG]-\d{3}$`
	// AnyCodePattern 是**解析既有号**的宽口径:P26-001 这类 2026-08 之前的历史号
	// 没有业务线字母(见 NAS 登记簿实况与 main.py:4517 的 PROJECT_CODE_RE)。
	// 导出给别的包用,别各自再写一份正则。
	AnyCodePattern = `^P26[CBG]?-\d{3}$`
	// ShortNamePattern 项目短名:1~8 位中英文或数字(照抄 main.py:4518 PROJECT_SHORT_NAME_RE)。
	ShortNamePattern = `^[A-Za-z0-9\x{3400}-\x{9fff}]{1,8}$`

	// codePrefix 2026 学年编号前缀(main.py:4447 PROJECT_CODE_PREFIX)。
	codePrefix = "P26"
)

var (
	codeRE      = regexp.MustCompile(CodePattern)
	anyCodeRE   = regexp.MustCompile(AnyCodePattern)
	shortNameRE = regexp.MustCompile(ShortNamePattern)
)

// Lines 是三条业务线:C=个人学生,B=机构,G=学校(main.py:4448 PROJECT_LINES)。
var Lines = []string{"C", "B", "G"}

var (
	// ErrCodeMoved 表示预告与占号之间号被别人拿走了 —— 调用方必须重新预告并让人再确认一次,
	// 绝不能把手里的旧号硬写进去。对应 NAS 侧的 409(main.py:4746)。
	ErrCodeMoved = errors.New("预告编号已变化,请重新预告并确认")
	// ErrNotEnabled 表示这条路径在当前阶段被有意关掉了(不是故障)。
	ErrNotEnabled = errors.New("该发号路径在当前迁移阶段未启用")
	// ErrBadLine 业务线不是 C/B/G。
	ErrBadLine = errors.New("业务线必须是 C、B 或 G")
	// ErrBadCode 编号格式不合法。
	ErrBadCode = errors.New("项目编号格式不合法")
	// ErrBadShortName 短名不合法。
	ErrBadShortName = errors.New("短名必须是 1 到 8 个中英文字母或数字")
	// ErrNoRegistry 本地登记簿不存在。发号是"唯一权威"型数据,宁可失败也不能凭空造一本新簿子
	// (那会从 001 重新发号,直接撞掉存量项目)。M5 冻结日必须先从 NAS 全量导入。
	ErrNoRegistry = errors.New("本地项目编号登记簿不存在")
	// ErrRegistryBroken 登记簿损坏,拒绝读写(对齐 NAS main.py:_load_project_codes 的 503 口径)。
	ErrRegistryBroken = errors.New("项目编号登记簿损坏，已停止写入")
)

// Request 是一次正式占号的请求。
type Request struct {
	// Line 业务线:C/B/G。
	Line string
	// ShortName 项目短名,1~8 位中英文或数字。
	ShortName string
	// ExpectedCode 是刚才 Preview 返回的编号。非空时:占号那一刻算出来的号必须与它一致,
	// 否则拒绝并返回 ErrCodeMoved(照抄 NASApp 确认卡「预告号必须原样带进确认卡」的规矩)。
	ExpectedCode string
}

// Issuer 是发号权威的统一入口。迁移期由 NAS 实现,M5 冻结日换成 Local。
type Issuer interface {
	// Preview 只读预告某条业务线的下一可用编号,不占号、不落盘。
	Preview(ctx context.Context, line string) (string, error)
	// Issue 正式占号并登记。req.ExpectedCode 非空时号变了必须拒绝(ErrCodeMoved)。
	Issue(ctx context.Context, req Request) (string, error)
}

// ValidCode 按严口径判断一个编号是否是合法的**新**编号。
func ValidCode(code string) bool { return codeRE.MatchString(code) }

// ValidAnyCode 按宽口径判断一个编号是否合法,历史上无业务线字母的号也算。
func ValidAnyCode(code string) bool { return anyCodeRE.MatchString(code) }

// ValidShortName 判断项目短名是否合法。
func ValidShortName(name string) bool { return shortNameRE.MatchString(name) }

// NormalizeLine 校验并标准化业务线(照抄 main.py:4594 _clean_project_line:去空格 + 转大写)。
func NormalizeLine(line string) (string, error) {
	clean := strings.ToUpper(strings.TrimSpace(line))
	for _, l := range Lines {
		if clean == l {
			return clean, nil
		}
	}
	return "", fmt.Errorf("%w(收到 %q)", ErrBadLine, line)
}

// SequenceOf 取编号的后三位序号。三条业务线共用同一个序号池,所以序号本身就够判重。
func SequenceOf(code string) (int, bool) {
	if !ValidAnyCode(code) {
		return 0, false
	}
	seq, err := strconv.Atoi(code[len(code)-3:])
	if err != nil {
		return 0, false
	}
	return seq, true
}

// formatCode 拼编号,与 main.py:4665 的 f"{PREFIX}{line}-{seq:03d}" 逐字对齐。
func formatCode(line string, seq int) string {
	return fmt.Sprintf("%s%s-%03d", codePrefix, line, seq)
}

// validateRequest 做一次与 NAS 侧同口径的入参体检(main.py:4728-4741)。
func validateRequest(req Request) (line string, err error) {
	line, err = NormalizeLine(req.Line)
	if err != nil {
		return "", err
	}
	if !ValidShortName(req.ShortName) {
		return "", fmt.Errorf("%w(收到 %q)", ErrBadShortName, req.ShortName)
	}
	if req.ExpectedCode != "" && !ValidCode(req.ExpectedCode) {
		return "", fmt.Errorf("%w:expected_code %q", ErrBadCode, req.ExpectedCode)
	}
	return line, nil
}
