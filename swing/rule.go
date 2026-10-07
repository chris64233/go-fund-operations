package swing

import (
	"fmt"
	"sort"
)

// SwingRule 摆动定价规则的一个不可修改版本。
// 发布后即冻结，任何修改都必须以新生效日期/新版本发布。
type SwingRule struct {
	FundID        string  // 基金代码
	EffectiveDate string  // 生效日期 YYYY-MM-DD（含当日）
	Version       int     // 该基金的规则版本号，从 1 递增
	Threshold     Decimal // 净资金流门槛（绝对值，严格大于才触发）
	UpFactor      Decimal // 净申购上调比例
	DownFactor    Decimal // 净赎回下调比例
	MaxAdjustment Decimal // 最大调整幅度（对上调/下调比例封顶）
}

// validate 校验规则参数合法性。
func (r SwingRule) validate() error {
	if r.FundID == "" {
		return fmt.Errorf("fund id is required")
	}
	if _, err := parseDate(r.EffectiveDate); err != nil {
		return fmt.Errorf("invalid effective date: %w", err)
	}
	if r.Threshold.Sign() < 0 {
		return fmt.Errorf("threshold must be non-negative")
	}
	if r.UpFactor.Sign() < 0 || r.DownFactor.Sign() < 0 {
		return fmt.Errorf("adjust factors must be non-negative")
	}
	if r.MaxAdjustment.Sign() <= 0 {
		return fmt.Errorf("max adjustment must be positive")
	}
	return nil
}

// parseDate 做简单的 YYYY-MM-DD 格式校验。
func parseDate(s string) (string, error) {
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		return "", fmt.Errorf("date %q not in YYYY-MM-DD format", s)
	}
	for i, c := range s {
		if i == 4 || i == 7 {
			continue
		}
		if c < '0' || c > '9' {
			return "", fmt.Errorf("date %q not in YYYY-MM-DD format", s)
		}
	}
	return s, nil
}

// ruleBook 按基金保存已发布的规则版本（只增不改）。
type ruleBook struct {
	rules map[string][]SwingRule
}

func newRuleBook() *ruleBook {
	return &ruleBook{rules: make(map[string][]SwingRule)}
}

// publish 发布新规则版本，返回带版本号的不可修改副本。
func (b *ruleBook) publish(rule SwingRule) (SwingRule, error) {
	if err := rule.validate(); err != nil {
		return SwingRule{}, err
	}
	versions := b.rules[rule.FundID]
	for _, v := range versions {
		if v.EffectiveDate == rule.EffectiveDate {
			return SwingRule{}, fmt.Errorf("fund %s already has a rule effective on %s (version %d); rules are immutable once published",
				rule.FundID, rule.EffectiveDate, v.Version)
		}
	}
	rule.Version = len(versions) + 1
	b.rules[rule.FundID] = append(versions, rule)
	return rule, nil
}

// effective 返回指定交易日当时有效的规则版本：
// 生效日期不晚于交易日的最新版本。
func (b *ruleBook) effective(fundID, tradeDate string) (SwingRule, bool) {
	versions := b.rules[fundID]
	var best *SwingRule
	for i := range versions {
		v := &versions[i]
		if v.EffectiveDate <= tradeDate && (best == nil || v.EffectiveDate > best.EffectiveDate) {
			best = v
		}
	}
	if best == nil {
		return SwingRule{}, false
	}
	return *best, true
}

// versions 返回某基金全部规则版本（按版本号升序的副本）。
func (b *ruleBook) versions(fundID string) []SwingRule {
	out := append([]SwingRule(nil), b.rules[fundID]...)
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out
}
