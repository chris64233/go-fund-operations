package fundoperations

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// ErrRuleNotFound 基金没有可用摆动规则。
var ErrRuleNotFound = errors.New("swing: no effective rule for fund and date")

// ErrRuleImmutable 已发布规则不可修改。
var ErrRuleImmutable = errors.New("swing: published rule is immutable")

// SwingRule 摆动定价规则的一个不可修改版本。
// 净资金流入（净申购）按 UpFactor 上调净值，净资金流出（净赎回）按
// DownFactor 下调净值，单次调整幅度不超过 MaxAdjustment。
type SwingRule struct {
	FundID        string
	Version       int
	EffectiveDate string // YYYY-MM-DD，自该交易日起生效
	Threshold     Decimal
	UpFactor      Decimal
	DownFactor    Decimal
	MaxAdjustment Decimal
	Published     bool
	PublishedAt   time.Time
}

func (r SwingRule) clone() *SwingRule { c := r; return &c }

// RuleStore 按基金保存规则版本，发布后不可修改。
type RuleStore struct {
	rules map[string][]*SwingRule // fundID -> 按版本升序
}

// NewRuleStore 创建空规则库。
func NewRuleStore() *RuleStore {
	return &RuleStore{rules: make(map[string][]*SwingRule)}
}

// CreateDraft 创建草稿版本，版本号自动递增。
func (s *RuleStore) CreateDraft(fundID, effectiveDate string, threshold, upFactor, downFactor, maxAdjustment Decimal) (*SwingRule, error) {
	if fundID == "" {
		return nil, errors.New("swing: fund id required")
	}
	if _, err := time.Parse("2006-01-02", effectiveDate); err != nil {
		return nil, fmt.Errorf("swing: invalid effective date %q", effectiveDate)
	}
	if threshold.Sign() < 0 || upFactor.Sign() < 0 || downFactor.Sign() < 0 || maxAdjustment.Sign() < 0 {
		return nil, errors.New("swing: rule parameters must be non-negative")
	}
	versions := s.rules[fundID]
	rule := &SwingRule{
		FundID:        fundID,
		Version:       len(versions) + 1,
		EffectiveDate: effectiveDate,
		Threshold:     threshold,
		UpFactor:      upFactor,
		DownFactor:    downFactor,
		MaxAdjustment: maxAdjustment,
	}
	s.rules[fundID] = append(versions, rule)
	return rule.clone(), nil
}

// Publish 发布指定版本，发布后该版本不可修改。
func (s *RuleStore) Publish(fundID string, version int) (*SwingRule, error) {
	rule := s.find(fundID, version)
	if rule == nil {
		return nil, fmt.Errorf("swing: rule %s v%d not found", fundID, version)
	}
	if rule.Published {
		return nil, fmt.Errorf("swing: rule %s v%d already published", fundID, version)
	}
	rule.Published = true
	rule.PublishedAt = time.Now()
	return rule.clone(), nil
}

// UpdateDraft 修改草稿版本；已发布版本返回 ErrRuleImmutable。
func (s *RuleStore) UpdateDraft(fundID string, version int, effectiveDate string, threshold, upFactor, downFactor, maxAdjustment Decimal) (*SwingRule, error) {
	rule := s.find(fundID, version)
	if rule == nil {
		return nil, fmt.Errorf("swing: rule %s v%d not found", fundID, version)
	}
	if rule.Published {
		return nil, ErrRuleImmutable
	}
	rule.EffectiveDate = effectiveDate
	rule.Threshold = threshold
	rule.UpFactor = upFactor
	rule.DownFactor = downFactor
	rule.MaxAdjustment = maxAdjustment
	return rule.clone(), nil
}

// EffectiveRule 返回交易日当天有效的已发布规则
// （生效日期不晚于交易日的最新发布版本）。
func (s *RuleStore) EffectiveRule(fundID, tradeDate string) (*SwingRule, error) {
	var best *SwingRule
	for _, r := range s.rules[fundID] {
		if !r.Published || r.EffectiveDate > tradeDate {
			continue
		}
		if best == nil || r.EffectiveDate > best.EffectiveDate ||
			(r.EffectiveDate == best.EffectiveDate && r.Version > best.Version) {
			best = r
		}
	}
	if best == nil {
		return nil, ErrRuleNotFound
	}
	return best.clone(), nil
}

// ListRules 返回基金全部规则版本（按版本升序）。
func (s *RuleStore) ListRules(fundID string) []*SwingRule {
	var out []*SwingRule
	for _, r := range s.rules[fundID] {
		out = append(out, r.clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out
}

func (s *RuleStore) find(fundID string, version int) *SwingRule {
	for _, r := range s.rules[fundID] {
		if r.Version == version {
			return r
		}
	}
	return nil
}
