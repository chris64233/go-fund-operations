# go-fund-operations

开放式基金交易与估值服务的 Go 项目基础模块。

开发环境：Go 1.23.0。

运行测试：

    go test ./...

## 摆动定价（Swing Pricing）

当基金当日净申购或净赎回超过配置门槛时，按资金流方向调整当日交易
净值，使交易成本由当日进出的投资者承担，保护存量持有人。

### 核心概念

- **规则版本**（`swing_rule.go`）：按基金与生效日期配置流量门槛、
  上调比例、下调比例与最大调整幅度。规则发布后不可修改
  （`ErrRuleImmutable`），日终计算通过 `EffectiveRule` 引用交易日
  当天有效的已发布版本。
- **日终计算**（`calculation.go`）：`Trial` 试算、`Confirm` 确认。
  计算时冻结当日有效申购、赎回与基础净值，先求净资金流
  （申购总额 − 赎回折算金额），绝对值超过门槛才触发摆动：
  净流入按上调比例调高净值，净流出按下调比例调低净值，
  单次调整不超过最大幅度。金额与净值均使用 `Decimal`
  （`decimal.go`，基于 `big.Rat`）精确表示。
- **确认与更正**：`Confirm` 后当日交易统一使用同一摆动净值，
  重复确认会被拒绝。确认后到达的申请自动标记为迟到、撤销的交易
  不会混入已确认快照；需要改正时调用 `Correct` 生成新计算版本与
  逐笔差额明细（`TradeDiff`），原版本保留为 `CalcSuperseded`，
  不被覆盖。
- **查询与解释**：`DiffVersions` 比较两个版本差异，
  `AffectedTrades` 给出某版本相对前一版本的受影响交易，
  `Explain` 输出某交易日是否触发摆动及完整计算依据。

### 示例

```go
rules := fundoperations.NewRuleStore()
trades := fundoperations.NewTradeStore()
eng := fundoperations.NewEngine(rules, trades)

rule, _ := rules.CreateDraft("F001", "2026-01-01",
    fundoperations.MustDecimal("1000000"), // 门槛
    fundoperations.MustDecimal("0.005"),   // 上调比例
    fundoperations.MustDecimal("0.008"),   // 下调比例
    fundoperations.MustDecimal("0.01"))    // 最大调整幅度
rules.Publish(rule.FundID, rule.Version)

eng.SubmitTrade(fundoperations.Trade{
    ID: "T1", FundID: "F001", TradeDate: "2026-03-02",
    Type: fundoperations.Subscription,
    Quantity: fundoperations.MustDecimal("2000000"),
})

calc, _ := eng.Confirm("F001", "2026-03-02", fundoperations.MustDecimal("1.5"))
// 净申购 200 万 > 门槛 100 万，触发上调：1.5 * 1.005 = 1.5075
fmt.Println(calc.Triggered, calc.SwungNAV) // true 1.5075

text, _ := eng.Explain("F001", "2026-03-02")
fmt.Println(text)
```
