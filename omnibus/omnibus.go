// Package omnibus 实现代销机构汇总交易的最终投资者分配。
//
// 机构先提交一笔总额汇总订单，再在截止前陆续补齐客户明细；
// 冻结时客户分配合计必须与汇总订单严格一致，否则整笔不能冻结。
// 订单确认、客户明细修改和冻结并发时以订单版本裁决。
package omnibus

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Direction 交易方向。
type Direction string

const (
	// DirectionSubscribe 申购，汇总订单以总金额计量。
	DirectionSubscribe Direction = "SUBSCRIBE"
	// DirectionRedeem 赎回，汇总订单以总份额计量。
	DirectionRedeem Direction = "REDEEM"
)

// Status 汇总订单状态。
type Status string

const (
	StatusOpen      Status = "OPEN"      // 接受客户明细
	StatusFrozen    Status = "FROZEN"    // 分配已冻结，等待确认
	StatusConfirmed Status = "CONFIRMED" // 已确认，客户持仓生效
	StatusFailed    Status = "FAILED"    // 机构订单失败，不产生份额
)

var (
	ErrOrderNotFound      = errors.New("omnibus: order not found")
	ErrVersionConflict    = errors.New("omnibus: order version conflict")
	ErrOrderNotOpen       = errors.New("omnibus: order is not open for allocation changes")
	ErrOrderNotFrozen     = errors.New("omnibus: order is not frozen")
	ErrInvalidCustomer    = errors.New("omnibus: invalid customer id")
	ErrInvalidQuantity    = errors.New("omnibus: allocation quantity must be positive")
	ErrAllocationMismatch = errors.New("omnibus: allocations do not match order total")
	ErrNoAllocations      = errors.New("omnibus: order has no allocations")
)

// Order 汇总订单：机构、基金、方向、总量、估值日与外部编号。
type Order struct {
	ID            string
	InstitutionID string
	FundID        string
	Direction     Direction
	TotalAmount   int64 // 申购总金额（最小货币单位）
	TotalShares   int64 // 赎回总份额（最小份额单位）
	ValuationDate string
	ExternalRef   string
	Version       int64
	Status        Status
}

// Total 返回订单的总量（申购为金额，赎回为份额）。
func (o *Order) Total() int64 {
	if o.Direction == DirectionRedeem {
		return o.TotalShares
	}
	return o.TotalAmount
}

// Allocation 客户分配明细。
type Allocation struct {
	CustomerID string
	Quantity   int64
	// OrderVersion 为该明细最后一次修改时的订单版本，用于并发裁决。
	OrderVersion int64
}

// FreezeRecord 冻结结果，包含稳定排序的分配快照。
type FreezeRecord struct {
	OrderID   string
	Version   int64
	Snapshot  []Allocation
	Total     int64
	Allocated int64
}

// Service 汇总交易分配服务，并发安全。
type Service struct {
	mu     sync.Mutex
	orders map[string]*orderState
	seq    int64
}

type orderState struct {
	order       Order
	allocations map[string]*Allocation // 有效分配按客户号唯一
	freeze      *FreezeRecord
	holdings    map[string]int64 // 确认后生效的客户持仓
}

func NewService() *Service {
	return &Service{orders: make(map[string]*orderState)}
}

// CreateOrder 登记汇总订单。相同机构与外部编号重复登记返回已有订单（幂等）。
func (s *Service) CreateOrder(institutionID, fundID string, direction Direction, total int64, valuationDate, externalRef string) (*Order, error) {
	if institutionID == "" || fundID == "" || externalRef == "" {
		return nil, fmt.Errorf("omnibus: institution, fund and external ref are required")
	}
	if total <= 0 {
		return nil, fmt.Errorf("omnibus: order total must be positive")
	}
	if direction != DirectionSubscribe && direction != DirectionRedeem {
		return nil, fmt.Errorf("omnibus: unknown direction %q", direction)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	key := institutionID + "|" + externalRef
	if st, ok := s.orders[key]; ok {
		dup := st.order
		return &dup, nil
	}
	s.seq++
	o := Order{
		ID:            fmt.Sprintf("OM%06d", s.seq),
		InstitutionID: institutionID,
		FundID:        fundID,
		Direction:     direction,
		ValuationDate: valuationDate,
		ExternalRef:   externalRef,
		Version:       1,
		Status:        StatusOpen,
	}
	if direction == DirectionRedeem {
		o.TotalShares = total
	} else {
		o.TotalAmount = total
	}
	st := &orderState{order: o, allocations: make(map[string]*Allocation)}
	s.orders[key] = st
	s.orders[o.ID] = st
	created := o
	return &created, nil
}

// UpsertAllocation 新增或修改客户分配。expectedVersion 为调用方看到的订单版本，
// 与当前版本不一致时返回 ErrVersionConflict，由版本裁决并发修改。
// 同一客户重复提交相同数量是幂等的；冻结后拒绝一切修改。
func (s *Service) UpsertAllocation(orderID, customerID string, quantity int64, expectedVersion int64) error {
	if customerID == "" {
		return ErrInvalidCustomer
	}
	if quantity <= 0 {
		return ErrInvalidQuantity
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.state(orderID)
	if err != nil {
		return err
	}
	if st.order.Status != StatusOpen {
		return ErrOrderNotOpen
	}
	if st.order.Version != expectedVersion {
		return ErrVersionConflict
	}
	if cur, ok := st.allocations[customerID]; ok && cur.Quantity == quantity {
		return nil // 幂等：相同内容不产生版本推进
	}
	st.allocations[customerID] = &Allocation{
		CustomerID:   customerID,
		Quantity:     quantity,
		OrderVersion: st.order.Version,
	}
	st.order.Version++
	return nil
}

// RemoveAllocation 撤销尚未冻结的客户明细。
func (s *Service) RemoveAllocation(orderID, customerID string, expectedVersion int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.state(orderID)
	if err != nil {
		return err
	}
	if st.order.Status != StatusOpen {
		return ErrOrderNotOpen
	}
	if st.order.Version != expectedVersion {
		return ErrVersionConflict
	}
	if _, ok := st.allocations[customerID]; !ok {
		return nil
	}
	delete(st.allocations, customerID)
	st.order.Version++
	return nil
}

// Freeze 冻结分配：客户明细总和必须与汇总订单严格一致，
// 存在缺口、超额或不合格客户时整笔不能冻结。
// 成功后保存按客户号稳定排序的分配快照。
func (s *Service) Freeze(orderID string, expectedVersion int64) (*FreezeRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.state(orderID)
	if err != nil {
		return nil, err
	}
	if st.order.Status == StatusFrozen && st.freeze != nil {
		if st.order.Version == expectedVersion {
			return cloneFreeze(st.freeze), nil // 幂等：同版本重复冻结返回既有快照
		}
		return nil, ErrVersionConflict
	}
	if st.order.Status != StatusOpen {
		return nil, ErrOrderNotOpen
	}
	if st.order.Version != expectedVersion {
		return nil, ErrVersionConflict
	}
	if len(st.allocations) == 0 {
		return nil, ErrNoAllocations
	}
	var sum int64
	for _, a := range st.allocations {
		if a.CustomerID == "" || a.Quantity <= 0 {
			return nil, ErrInvalidCustomer
		}
		sum += a.Quantity
	}
	total := st.order.Total()
	if sum != total {
		return nil, fmt.Errorf("%w: allocated %d, order total %d, gap %d",
			ErrAllocationMismatch, sum, total, total-sum)
	}
	snapshot := make([]Allocation, 0, len(st.allocations))
	for _, a := range st.allocations {
		snapshot = append(snapshot, *a)
	}
	sort.Slice(snapshot, func(i, j int) bool { return snapshot[i].CustomerID < snapshot[j].CustomerID })
	st.order.Version++
	st.order.Status = StatusFrozen
	st.freeze = &FreezeRecord{
		OrderID:   st.order.ID,
		Version:   st.order.Version,
		Snapshot:  snapshot,
		Total:     total,
		Allocated: sum,
	}
	return cloneFreeze(st.freeze), nil
}

// Confirm 确认机构订单。success 为 false 时订单失败：
// 客户明细保留查询，但不生成任何份额。
func (s *Service) Confirm(orderID string, expectedVersion int64, success bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.state(orderID)
	if err != nil {
		return err
	}
	if st.order.Status != StatusFrozen {
		return ErrOrderNotFrozen
	}
	if st.order.Version != expectedVersion {
		return ErrVersionConflict
	}
	st.order.Version++
	if !success {
		st.order.Status = StatusFailed
		return nil
	}
	st.order.Status = StatusConfirmed
	st.holdings = make(map[string]int64, len(st.freeze.Snapshot))
	for _, a := range st.freeze.Snapshot {
		st.holdings[a.CustomerID] += a.Quantity
	}
	return nil
}

// Gap 返回汇总订单总量与当前有效分配合计的差额（正数为缺口，负数为超额）。
func (s *Service) Gap(orderID string) (total, allocated, gap int64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.state(orderID)
	if err != nil {
		return 0, 0, 0, err
	}
	for _, a := range st.allocations {
		allocated += a.Quantity
	}
	total = st.order.Total()
	return total, allocated, total - allocated, nil
}

// GetOrder 查询汇总订单。
func (s *Service) GetOrder(orderID string) (*Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.state(orderID)
	if err != nil {
		return nil, err
	}
	o := st.order
	return &o, nil
}

// Allocations 查询当前有效客户分配（按客户号排序）。失败的订单仍可查询。
func (s *Service) Allocations(orderID string) ([]Allocation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.state(orderID)
	if err != nil {
		return nil, err
	}
	out := make([]Allocation, 0, len(st.allocations))
	for _, a := range st.allocations {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CustomerID < out[j].CustomerID })
	return out, nil
}

// FreezeRecordOf 查询冻结快照。
func (s *Service) FreezeRecordOf(orderID string) (*FreezeRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.state(orderID)
	if err != nil {
		return nil, err
	}
	if st.freeze == nil {
		return nil, ErrOrderNotFrozen
	}
	return cloneFreeze(st.freeze), nil
}

// Holding 查询客户已确认持仓；订单未确认或已失败时恒为 0。
func (s *Service) Holding(orderID, customerID string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.state(orderID)
	if err != nil {
		return 0, err
	}
	if st.order.Status != StatusConfirmed {
		return 0, nil
	}
	return st.holdings[customerID], nil
}

func (s *Service) state(orderID string) (*orderState, error) {
	st, ok := s.orders[orderID]
	if !ok {
		return nil, ErrOrderNotFound
	}
	return st, nil
}

func cloneFreeze(f *FreezeRecord) *FreezeRecord {
	cp := *f
	cp.Snapshot = append([]Allocation(nil), f.Snapshot...)
	return &cp
}
