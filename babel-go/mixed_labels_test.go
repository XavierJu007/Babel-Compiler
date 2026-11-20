package main

import (
	"strings"
	"testing"
)

func TestMixedApprovalScopesReachBothValidationModes(t *testing.T) {
	const original = "单笔不超过99元，一线客服可直接处理，无需审批；99元至500元，需主管审批。"
	const higherTier = "99元至500元:主管须审批"
	for _, exemption := range []string{"no approval", "No审批"} {
		lowerTier := "≤99元:一线客服可→直接处理," + exemption
		candidate := lowerTier + "; " + higherTier
		reading := BlindReading{
			SchemaVersion: 1, Entities: []string{"一线客服", "主管"}, Ambiguities: []string{},
			Relations: []BlindRelation{
				{Subject: "一线客服", Relation: "直接处理", Conditions: []string{"单笔≤99元"}, Modality: "permission",
					Quantities: []BlindQuantity{{Value: "99", Unit: "元", Comparison: "≤", AppliesTo: "单笔金额"}}, Evidence: lowerTier},
				{Relation: "审批", Conditions: []string{"单笔≤99元"}, Modality: "exemption",
					Quantities: []BlindQuantity{{Value: "99", Unit: "元", Comparison: "≤", AppliesTo: "单笔金额"}}, Evidence: lowerTier},
				{Subject: "主管", Relation: "审批", Conditions: []string{"单笔99元至500元"}, Modality: "obligation",
					Quantities: []BlindQuantity{{Value: "99", Unit: "元", AppliesTo: "金额区间"}, {Value: "500", Unit: "元", AppliesTo: "金额区间"}}, Evidence: higherTier},
			},
		}
		for _, mode := range []string{"compare", "blind"} {
			t.Run(exemption+"/"+mode, func(t *testing.T) {
				// Handwritten HTTP fixtures prove routing and grounding, not that
				// a real model understands the mixed wording or resolves the overlap.
				runQuotedSemanticFixture(t, mode, original, candidate, reading, true)
			})
		}
	}
}

func TestMixedLabelsDoNotRelaxSameScopeOrNumericGuards(t *testing.T) {
	for _, test := range []struct{ name, original, candidate string }{
		{"same scope obligation", "单笔≤99元:无需审批。", "单笔≤99元:必须审批。"},
		{"same scope permission", "内部操作:允许退款", "内部操作:禁止退款"},
		{"unscoped obligation", "无需审批", "须审批"},
		{"changed threshold", "单笔≤99元:无需审批。", "单笔≤98元:no approval。"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if issues := checkDeterministicInvariants(test.original, test.candidate, nil); len(issues) == 0 {
				t.Fatal("explicit same-scope conflict or value mutation escaped local checks")
			}
		})
	}
}

func TestMixedInheritedScopesReachBothValidationModes(t *testing.T) {
	for _, tc := range []struct {
		name, original, candidate, action, lowerCondition, upperCondition, lowerModality, upperModality string
	}{
		{"indented exemption", "金额≤99元:\n  无需审批\n金额>99元:\n  必须审批",
			"金额≤99元:\n  No审批\n金额>99元:\n  必须审批", "审批", "金额≤99元", "金额>99元", "exemption", "obligation"},
		{"indented permission", "内部操作:\n  允许退款\n外部操作:\n  禁止退款",
			"内部操作:\n  可退款\n外部操作:\n  禁止退款", "退款", "内部操作", "外部操作", "permission", "prohibition"},
		{"same depth different parents", "受理:\n  普通订单:\n    无需审批\n  风险订单:\n    须审批",
			"受理:\n  普通订单:\n    No审批\n  风险订单:\n    须审批", "审批", "普通订单", "风险订单", "exemption", "obligation"},
		{"tree branches", "受理\n├─ 普通订单\n│  └─ 无需审批\n└─ 风险订单\n   └─ 须审批",
			"受理\n├─ 普通订单\n│  └─ No审批\n└─ 风险订单\n   └─ 须审批", "审批", "普通订单", "风险订单", "exemption", "obligation"},
		{"comma separated conditions", "金额≤99元，无需审批；金额>99元，必须审批",
			"金额≤99元，No审批；金额>99元，必须审批", "审批", "金额≤99元", "金额>99元", "exemption", "obligation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if issues := checkDeterministicInvariants(tc.original, tc.candidate, nil); len(issues) != 0 {
				t.Fatalf("different inherited conditions were treated as one local scope: %v", issues)
			}
			reading := BlindReading{SchemaVersion: 1, Entities: []string{tc.action}, Ambiguities: []string{},
				Relations: []BlindRelation{
					{Relation: tc.action, Conditions: []string{tc.lowerCondition}, Modality: tc.lowerModality,
						Quantities: []BlindQuantity{}, Evidence: tc.candidate},
					{Relation: tc.action, Conditions: []string{tc.upperCondition}, Modality: tc.upperModality,
						Quantities: []BlindQuantity{}, Evidence: tc.candidate},
				}}
			if strings.Contains(tc.lowerCondition, "99") {
				reading.Relations[0].Quantities = []BlindQuantity{{Value: "99", Unit: "元", Comparison: "≤", AppliesTo: "金额"}}
				reading.Relations[1].Quantities = []BlindQuantity{{Value: "99", Unit: "元", Comparison: ">", AppliesTo: "金额"}}
			}
			for _, mode := range []string{"compare", "blind"} {
				for _, pass := range []bool{true, false} {
					verdict := "model rejects"
					if pass {
						verdict = "model accepts"
					}
					t.Run(mode+"/"+verdict, func(t *testing.T) {
						// Evidence deliberately includes both branches and their parent
						// headings. An obligation elsewhere in that context is not a
						// contradiction of this relation's exemption or permission.
						runQuotedSemanticFixture(t, mode, tc.original, tc.candidate, reading, pass)
					})
				}
			}
		})
	}
}

func TestMixedLabelTokenMeasurements(t *testing.T) {
	for _, encoding := range []string{"cl100k_base", "o200k_base"} {
		t.Run(encoding, func(t *testing.T) {
			for _, group := range [][]string{
				{"角色", "role", "角色:", "role:"},
				{"目标", "goal", "目标:", "goal:"},
				{"无需审批", "No审批", "no approval"},
				{"如果库存服务不可用，则挂起退款并告警", "if库存服务不可用⇒挂起退款∧告警", "库存服务不可用→挂起退款+告警"},
				{"因为库存服务不可用，所以挂起退款并告警", "挂起退款∧告警∵库存服务不可用"},
				{"如果", "if", "因为", "∵", "因此", "⇒"},
			} {
				for _, text := range group {
					count, err := measureTokens(text, encoding)
					if err != nil {
						t.Fatal(err)
					}
					t.Logf("%q: %d text tokens", text, count.Tokens)
				}
			}
			// These default labels have a public meaning and do not cost more
			// in either supported encoding. Other substitutions are measured,
			// not assumed to save tokens or to express an equivalent relation.
			for _, pair := range [][2]string{{"角色:", "role:"}, {"目标:", "goal:"}} {
				before, err := measureTokens(pair[0], encoding)
				if err != nil {
					t.Fatal(err)
				}
				after, err := measureTokens(pair[1], encoding)
				if err != nil {
					t.Fatal(err)
				}
				if after.Tokens > before.Tokens {
					t.Fatalf("default label increases tokens: %s", strings.Join(pair[:], " -> "))
				}
			}
		})
	}
}
