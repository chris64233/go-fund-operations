# go-fund-operations

开放式基金交易与估值服务的 Go 项目基础模块。

开发环境：Go 1.23.0。

运行测试：

    go test ./...

## 摆动定价（swing 包）

`swing` 包实现基金交易日的摆动定价：当净申购或净赎回超过门槛时，
按当日资金流方向调整交易净值，让交易成本由当日进出投资者承担。

### 核心概念

- **规则版本不可修改**：`PublishRule` 按基金和生效日期发布流量门槛、
  上调比例、下调比例和最大调整幅度。发布后形成不可修改版本
  （同一生效日期不允许重复发布），日终计算通过 `EffectiveRule`
  引用交易日当时有效的版本。
- **日终计算**：`TrialCalculate`（试算）与 `Confirm`（确认）冻结当日
  有效申购、赎回和基础净值，先得出净资金流（申购 − 赎回），
  再判断是否触发摆动：净资金流绝对值**严格大于**门槛才触发；
  净流入按上调比例调高净值，净流出按下调比例调低净值，
  调整比例受最大调整幅度封顶。所有金额与净值使用 `Decimal`
  （整数尾数 + 十进制标度）精确表示，无浮点误差。
- **确认与更正**：`Confirm` 后当日交易统一使用同一摆动净值，
  重复确认会被拒绝。确认后到达的迟到申请、确认后撤销的交易
  不会混入已确认快照；调用 `Correct` 生成新的计算版本并输出
  每笔交易的差额明细（`TradeDelta`），原确认版本保持不变。
- **查询**：
  - `ListRuleVersions` / `EffectiveRule`：规则维护与当时有效版本；
  - `GetCalculation`：按版本查询计算结果（0 表示最新）；
  - `VersionDiff`：两个计算版本的差异说明；
  - `AffectedTrades`：受影响交易，标注迟到/撤销待更正的申请；
  - `Explain`：说明某交易日是否触发摆动及其完整计算依据。

### 示例

```go
s := swing.NewService()
s.PublishRule(swing.SwingRule{
    FundID: "FUND01", EffectiveDate: "2026-01-01",
    Threshold: swing.MustDecimal("1000000"),
    UpFactor: swing.MustDecimal("0.005"),
    DownFactor: swing.MustDecimal("0.008"),
    MaxAdjustment: swing.MustDecimal("0.01"),
})
s.AddTrade(swing.Trade{ID: "T1", FundID: "FUND01", TradeDate: "2026-03-02",
    Type: swing.Subscription, Amount: swing.MustDecimal("2000000")})
calc, _ := s.Confirm("FUND01", "2026-03-02", swing.MustDecimal("1.2345"))
// calc.Triggered == true, calc.Direction == swing.DirectionUp
// calc.AdjustedNAV == 1.2406725（1.2345 × 1.005，精确表示）
```

### 测试

`swing/swing_test.go` 覆盖：精确十进制运算、规则不可修改与有效版本
选择、门槛边界（等于不触发、刚好超过触发）、上调/下调两个方向、
最大调整幅度封顶、重复确认拒绝、迟到交易不混入快照、更正生成
新版本与差额明细、计算依据说明。
