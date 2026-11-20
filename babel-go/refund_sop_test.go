package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"
)

// This is a handwritten expression target, not a model-generated answer or a
// proof of semantic equivalence. Keep its complete source alongside it so the
// overlapping amount tiers, unusual retry order and stray input remain visible.
func refundSOPFixture(t *testing.T) (string, string) {
	t.Helper()
	return readRefundFixture(t, "refund-sop.source.txt"), readRefundFixture(t, "refund-sop.babel.txt")
}

func readRefundFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "docs", "examples", name))
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.Valid(data) {
		t.Fatalf("fixture %s is not UTF-8", name)
	}
	return strings.TrimSuffix(string(data), "\n")
}

func TestRefundSOPRelationFixture(t *testing.T) {
	source, candidate := refundSOPFixture(t)
	// These explicit fixture checks document facts that a lexical gate alone
	// cannot prove, including fixed return values, labels and the input oddity.
	for _, literal := range []string{
		"「处理中」", "「余额不足」", "「账户异常」", "「合规」",
		"「流程瑕疵」", "「资金风险」", "「待确认」", "09:00", "SOP", "SKU", "å",
	} {
		if !strings.Contains(source, literal) || !strings.Contains(candidate, literal) {
			t.Errorf("source/candidate fixture lost literal %q", literal)
		}
	}
	// These are quoted business descriptions, not machine strings. Exercise a
	// real relation rewrite instead of hiding overprotection by copying them.
	for _, phrase := range []string{"「已签收未超过 7 天」", "「满 3 件 8 折」"} {
		if !strings.Contains(source, phrase) || strings.Contains(candidate, phrase) {
			t.Fatalf("fixture no longer exercises quoted description compression: %q", phrase)
		}
	}
	if !strings.Contains(candidate, "role:") || !strings.Contains(candidate, "goal[") || !strings.Contains(candidate, "├") || !strings.Contains(candidate, "╰") {
		t.Fatal("fixture lost its self-explanatory mixed labels or scoped tree")
	}
	if titles := inheritedOutlineTitles(source, candidate); len(titles) != 0 {
		t.Fatalf("reference retained the source chapter layout: %v", titles)
	}
	if issues := checkDeterministicInvariants(source, candidate, nil); len(issues) != 0 {
		t.Fatalf("refund relation fixture rejected before model comparison: %v", issues)
	}
	if got := compactDSL(candidate); got != candidate {
		t.Fatal("postprocessing changed the refund relation fixture")
	}
}

func TestRefundSOPScopedConstraints(t *testing.T) {
	_, candidate := refundSOPFixture(t)
	// Check facts, modalities and their local binding, rather than requiring the
	// earlier sentence-based wording. These checks protect this handwritten target;
	// they are not a semantic proof or a substitute for real model comparison.
	for _, test := range []struct{ name, pattern string }{
		{"funds before speed", `goal\[.*商家∧平台资金安全前提\].*≤48小时.*库存\|优惠券\|积分\|发票.*一致`},
		{"qualification disjunction", `自助许可:.*\(已支付∧未发货\)∨\(签收(?:后)?≤7天∧(?:商品)?未拆封\)`},
		{"promotion exclusion conjunction", `禁自助:.*\(参与.*满3件8折∧已拆单\)`},
		{"conditional human approval", `签收>7天.*≤15天.*须.*凭证.*∧.*售后主管审批`},
		{"appeal only", `>15天.*禁.*直接退款.*仅.*特殊申诉.*抄送财务`},
		{"shipping prerequisite", `单笔退款≤实付.*含运费订单仅退商品.*运费可退仅if商家责任.*须备注责任方`},
		{"low tier permission", `≤99元.*一线客服.*(?:可|may).*直接处理.*(?:no approval|No审批|无需审批)`},
		{"overlap at 99 and escalation", `99元至500元.*须主管审批.*审批超时2小时.*自动.*售后经理`},
		{"overlap at 500 and both signatures", `500元至2000元.*须.*售后经理∧财务双签`},
		{"offline only", `>2000元.*须线下复核会签.*禁止线上直接放款`},
		{"request limits", `同订单30天.*≤3次.*同用户24小时.*>5笔.*自动.*人工抽检队列.*≥20%`},
		{"execution order", `flow:.*锁订单→校验权益→回滚库存→发起退款→冲销账务→通知用户`},
		{"lock and idempotency", `锁订单:.*操作人\|时间戳\|退款原因码\|来源渠道.*锁内禁并发改金额.*幂等键去重\(300秒\)`},
		{"single lock winner", `同订单并发≥2.*仅首个可过锁.*余返「处理中」.*禁并行扣减`},
		{"rounding and entitlement", `权益:按比例分摊\{优惠券\|满减\|积分抵扣\|运费险\}.*四舍五入2位小数.*退款总额≤实付.*优惠券仅返仍有效部分.*过期不返`},
		{"stock exception ambiguity retained", `库存:.*库存服务.*可售数量.*权益类商品\[已拆封、已激活\].*不回滚/仅作废.*组合商品∀子SKU分别回滚`},
		{"unusual stock retry order", `∃子项失败.*整单回滚成功后再重试3次\(间隔5秒\)`},
		{"stock outage blocks refund", `if库存服务不可用.*先挂起退款\+告警.*禁跳库存直接退款`},
		{"invoice before refund", `退款前:发票已开具.*须先作废→退款.*作废失败.*挂起\+告警`},
		{"payment timeout", `退款:优先原路.*渠道超时30秒.*「处理中」.*禁重复发起第二次退款请求.*「处理中」>2小时.*人工核对渠道流水`},
		{"terminal payment errors", `渠道终态错.*「余额不足」.*「账户异常」.*不自动重试.*直接转人工.*渠道原始错误码`},
		{"accounting deadline and mismatch", `退款成功后10分钟内须生成红字凭证.*支付流水\+发票状态三方对账.*差异>0.01元.*暂停自动流程.*差异明细→财务.*禁止自行调账`},
		{"notification limits and redaction", `通知用户.*防重复打扰.*统一模板.*手机号\|银行卡号\|身份证号.*仅后4位.*短信≤1次∧站内信≤1次`},
		{"audit snapshots and retention", `∀步骤.*可追溯记录.*∀写操作.*前∧后状态快照\{订单号\|退款单号\|金额\|状态\|操作人\|时间戳\}.*≥180天`},
		{"throttling", `限流20次/秒.*超出.*指数退避重试\(最长等待120秒\).*仍失败.*异步队列`},
		{"daily report and quality trigger", `每日09:00.*前日退款日报\{受理量\|通过率\|平均处理时长\|超时单量\|差异单量\}.*通过率<95%∨超时率>3%.*质检抽样`},
		{"risk review", `质检结论∈\{「合规」\|「流程瑕疵」\|「资金风险」\}.*资金风险样本.*须24小时内复盘.*同批次50单`},
		{"report and screenshot privacy", `日报/工单用户信息∀脱敏.*截图打码\{手机号\|地址\|订单号后6位\}.*原始明细导出须审批`},
		{"ticket isolation", `单用户可识别信息仅当前工单使用.*禁止\{跨工单检索\|批量导出\|转发至外部沟通工具\}`},
		{"scope and unknown cases", `禁止\{改商品价格\|调店铺设置\|介入营销活动配置\}.*场景∉SOP覆盖.*先挂起.*登记「待确认」.*禁止.*规则后放款`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if !regexp.MustCompile(test.pattern).MatchString(candidate) {
				t.Fatalf("fixture lost a value, modality or scoped relationship: %s", test.pattern)
			}
		})
	}
}

func TestRefundSOPTokenMeasurements(t *testing.T) {
	source, candidate := refundSOPFixture(t)
	reported := readRefundFixture(t, "refund-sop.reported.txt")
	for _, encoding := range []string{"cl100k_base", "o200k_base"} {
		t.Run(encoding, func(t *testing.T) {
			before, err := measureTokens(source, encoding)
			if err != nil {
				t.Fatal(err)
			}
			after, err := measureTokens(candidate, encoding)
			if err != nil {
				t.Fatal(err)
			}
			previous, err := measureTokens(reported, encoding)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s text tokens: source=%d; user-reported=%d; handwritten=%d; source/candidate runes=%d/%d",
				encoding, before.Tokens, previous.Tokens, after.Tokens, utf8.RuneCountInString(source), utf8.RuneCountInString(candidate))
			if after.Tokens >= before.Tokens || after.Tokens >= previous.Tokens {
				t.Fatal("handwritten tree is no shorter than both source and reported output in this encoding")
			}
		})
	}
}

func TestRefundSOPKnownLiteralAndNumericDamageIsRejected(t *testing.T) {
	source, candidate := refundSOPFixture(t)
	if issues := checkDeterministicInvariants(source, candidate, nil); len(issues) != 0 {
		t.Fatalf("cannot attribute rejection to a mutation while its baseline is rejected: %v", issues)
	}
	for _, mutation := range []struct{ name, before, after string }{
		{"quoted processing state", "「处理中」", "「已完成」"},
		{"idempotency duration", "300秒", "301秒"},
		{"retention minimum", "≥180天", "<180天"},
		{"reconciliation threshold", ">0.01元", ">0.02元"},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			if !strings.Contains(candidate, mutation.before) {
				t.Fatal("mutation no longer targets the candidate")
			}
			broken := strings.ReplaceAll(candidate, mutation.before, mutation.after)
			if issues := checkDeterministicInvariants(source, broken, nil); len(issues) == 0 {
				t.Fatal("known literal or numeric damage was not rejected")
			}
		})
	}
}

func TestRefundSOPScopedModalDamageReachesComparison(t *testing.T) {
	source, candidate := refundSOPFixture(t)
	broken := strings.Replace(candidate, "禁止线上直接放款", "允许线上直接放款", 1)
	if broken == candidate {
		t.Fatal("mutation no longer targets the candidate")
	}
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeModelContent(w, `{"pass":false,"missing":["超过2000元的禁止线上直接放款被改成许可"]}`)
	}))
	defer provider.Close()
	// A tree branch inherits its amount condition. Local lexical matching must
	// not equate unknown parent scopes; deferral still requires semantic review.
	result := runValidation(Profile{APIKey: "fixture", BaseURL: provider.URL}, source, broken, nil)
	if calls.Load() != 1 || result.Status != "failed" || result.Method != "llm_compare" {
		t.Fatalf("scoped modal damage bypassed comparison: calls=%d result=%+v", calls.Load(), result)
	}
}
