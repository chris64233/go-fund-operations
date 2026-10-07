package fundoperations

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Direction 表示汇总订单的交易方向。
type Direction string

const (
	DirectionSubscribe Direction = "SUBSCRIBE" // 申购，按总金额
	DirectionRedeem    Direction = "REDEEM"    // 赎回，按总份额
)

// OrderStatus 表示汇总订单的生命周期状态。
type OrderStatus string

const (
	OrderStatusOpen      OrderStatus = "OPEN"      // 可补录/修改客户明细
	OrderStatusFrozen    OrderStatus = "FROZEN"    // 分配已冻结，明细不可再改
	OrderStatusConfirmed OrderStatus = "CONFIRMED" // 已确认，客户持仓已生成
	OrderStatusFailed    OrderStatus = "FAILED"    // 机构订单失败，明细仅可查询
)

var (
	ErrOrderNotFound      = errors.New("omnibus: order not found")
	ErrVersionConflict    = errors.New("omnibus: order version conflict")
	ErrOrderNotOpen       = errors.New("omnibus: order is not open for allocation changes")
	ErrOrderNotFrozen     = errors.New("omnibus: order is not frozen")
	ErrOrderNotActive     = errors.New("omnibus: order is not active")
	ErrInvalidQuantity    = errors.New("omnibus: quantity must be positive")
	ErrInvalidTotal       = errors.New("omnibus: order total must be positive")
	ErrIneligibleClient   = errors.New("omnibus: client is not eligible")
	ErrAllocationMismatch = errors.New("omnibus: allocation sum does not match order total")
	ErrEmptyClientID      = errors.New("omnibus: client id must not be empty")
)

// OmnibusOrder 是代销机构提交的一笔汇总订单。
type OmnibusOrder struct {
	ID            string
	InstitutionID string
	FundID        string
	Direction     Direction
	// TotalAmount 为申购总金额（DirectionSubscribe 时使用）。
	TotalAmount int64
	// TotalShares 为赎回总份额（DirectionRedeem 时使用）。
	TotalShares int64
	NavDate     time.Time
	ExternalRef string
	Status      OrderStatus
	Version     int64
}

// Total 返回与交易方向对应的订单总量（金额或份额）。
func (o *OmnibusOrder) Total() int64 {
	if o.Direction == DirectionRedeem {
		return o.TotalShares
	}
	return o.TotalAmount
}

// Allocation 是一条客户分配明细。
type Allocation struct {
	ClientID string
	Quantity int64
}

// AllocationSnapshot 是冻结时保存的稳定排序分配快照。
type AllocationSnapshot struct {
	OrderID     string
	FrozenAt    time.Time
	Version     int64
	Allocations []Allocation // 按 ClientID 升序排序
}

// Position 是确认后生成的客户持仓。
type Position struct {
	ClientID string
	FundID   string
	Shares   int64
}

// GapReport 描述汇总订单与客户分配之间的差额。
type GapReport struct {
	OrderID     string
	OrderTotal  int64
	Allocated   int64
	Gap         int64 // 正数表示缺口，负数表示超额
	ClientCount int
}

// OmnibusService 管理汇总订单、客户分配、冻结与确认。
type OmnibusService struct {
	mu         sync.Mutex
	orders     map[string]*OmnibusOrder
	allocs     map[string]map[string]int64 // orderID -> clientID -> quantity
	snapshots  map[string]*AllocationSnapshot
	positions  map[string][]Position // orderID -> positions
	eligible   map[string]bool
	now        func() time.Time
	idSequence int64
}

// NewOmnibusService 创建一个空的汇总交易服务。
func NewOmnibusService() *OmnibusService {
	return &OmnibusService{
		orders:    make(map[string]*OmnibusOrder),
		allocs:    make(map[string]map[string]int64),
		snapshots: make(map[string]*AllocationSnapshot),
		positions: make(map[string][]Position),
		eligible:  make(map[string]bool),
		now:       time.Now,
	}
}

// RegisterEligibleClient 登记合格客户；未登记的客户不能参与分配。
func (s *OmnibusService) RegisterEligibleClient(clientID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.eligible[clientID] = true
}

// SubmitOrder 登记一笔机构汇总订单并返回订单 ID。
func (s *OmnibusService) SubmitOrder(institutionID, fundID string, direction Direction, total int64, navDate time.Time, externalRef string) (string, error) {
	if total <= 0 {
		return "", ErrInvalidTotal
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.idSequence++
	id := fmt.Sprintf("OM%06d", s.idSequence)
	order := &OmnibusOrder{
		ID:            id,
		InstitutionID: institutionID,
		FundID:        fundID,
		Direction:     direction,
		NavDate:       navDate,
		ExternalRef:   externalRef,
		Status:        OrderStatusOpen,
		Version:       1,
	}
	if direction == DirectionRedeem {
		order.TotalShares = total
	} else {
		order.TotalAmount = total
	}
	s.orders[id] = order
	s.allocs[id] = make(map[string]int64)
	return id, nil
}

// UpsertAllocation 新增或修改某客户的分配数量。
// 同一客户仅存在一条有效分配，重复提交视为修改；expectedVersion 用于并发裁决。
func (s *OmnibusService) UpsertAllocation(orderID, clientID string, quantity int64, expectedVersion int64) error {
	if clientID == "" {
		return ErrEmptyClientID
	}
	if quantity <= 0 {
		return ErrInvalidQuantity
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	order, err := s.lockOrderForUpdate(orderID, expectedVersion)
	if err != nil {
		return err
	}
	if !s.eligible[clientID] {
		return ErrIneligibleClient
	}
	s.allocs[orderID][clientID] = quantity
	order.Version++
	return nil
}

// RemoveAllocation 删除尚未冻结的客户分配。
func (s *OmnibusService) RemoveAllocation(orderID, clientID string, expectedVersion int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	order, err := s.lockOrderForUpdate(orderID, expectedVersion)
	if err != nil {
		return err
	}
	delete(s.allocs[orderID], clientID)
	order.Version++
	return nil
}

// lockOrderForUpdate 校验订单存在、版本一致且处于可修改状态。
func (s *OmnibusService) lockOrderForUpdate(orderID string, expectedVersion int64) (*OmnibusOrder, error) {
	order, ok := s.orders[orderID]
	if !ok {
		return nil, ErrOrderNotFound
	}
	if order.Version != expectedVersion {
		return nil, ErrVersionConflict
	}
	if order.Status != OrderStatusOpen {
		return nil, ErrOrderNotOpen
	}
	return order, nil
}

// Freeze 校验客户明细总和与汇总订单严格一致后冻结分配，
// 并保存按客户号升序排序的稳定快照。缺口、超额或存在不合格客户时整笔拒绝。
func (s *OmnibusService) Freeze(orderID string, expectedVersion int64) (*AllocationSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	order, ok := s.orders[orderID]
	if !ok {
		return nil, ErrOrderNotFound
	}
	if order.Version != expectedVersion {
		return nil, ErrVersionConflict
	}
	switch order.Status {
	case OrderStatusFrozen:
		// 幂等：相同版本重复冻结直接返回既有快照。
		return s.snapshots[orderID], nil
	case OrderStatusOpen:
	default:
		return nil, ErrOrderNotActive
	}
	allocs := s.allocs[orderID]
	var sum int64
	for clientID, qty := range allocs {
		if !s.eligible[clientID] {
			return nil, fmt.Errorf("%w: %s", ErrIneligibleClient, clientID)
		}
		sum += qty
	}
	if sum != order.Total() {
		return nil, fmt.Errorf("%w: allocated %d, order total %d", ErrAllocationMismatch, sum, order.Total())
	}
	snapshot := &AllocationSnapshot{
		OrderID:  orderID,
		FrozenAt: s.now(),
	}
	for clientID, qty := range allocs {
		snapshot.Allocations = append(snapshot.Allocations, Allocation{ClientID: clientID, Quantity: qty})
	}
	sort.Slice(snapshot.Allocations, func(i, j int) bool {
		return snapshot.Allocations[i].ClientID < snapshot.Allocations[j].ClientID
	})
	order.Status = OrderStatusFrozen
	order.Version++
	snapshot.Version = order.Version
	s.snapshots[orderID] = snapshot
	return snapshot, nil
}

// Confirm 确认已冻结的订单，按冻结快照生成客户持仓。
// 重复确认幂等返回既有持仓。
func (s *OmnibusService) Confirm(orderID string, expectedVersion int64) ([]Position, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	order, ok := s.orders[orderID]
	if !ok {
		return nil, ErrOrderNotFound
	}
	if order.Version != expectedVersion {
		return nil, ErrVersionConflict
	}
	if order.Status == OrderStatusConfirmed {
		return append([]Position(nil), s.positions[orderID]...), nil
	}
	if order.Status != OrderStatusFrozen {
		return nil, ErrOrderNotFrozen
	}
	snapshot := s.snapshots[orderID]
	positions := make([]Position, 0, len(snapshot.Allocations))
	for _, a := range snapshot.Allocations {
		positions = append(positions, Position{
			ClientID: a.ClientID,
			FundID:   order.FundID,
			Shares:   a.Quantity,
		})
	}
	order.Status = OrderStatusConfirmed
	order.Version++
	s.positions[orderID] = positions
	return append([]Position(nil), positions...), nil
}

// Fail 将订单标记为失败。客户明细保留可查询，但不会生成任何持仓。
func (s *OmnibusService) Fail(orderID string, expectedVersion int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	order, ok := s.orders[orderID]
	if !ok {
		return ErrOrderNotFound
	}
	if order.Version != expectedVersion {
		return ErrVersionConflict
	}
	if order.Status == OrderStatusFailed {
		return nil
	}
	if order.Status == OrderStatusConfirmed {
		return ErrOrderNotActive
	}
	order.Status = OrderStatusFailed
	order.Version++
	return nil
}

// GetOrder 返回订单的只读副本。
func (s *OmnibusService) GetOrder(orderID string) (OmnibusOrder, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	order, ok := s.orders[orderID]
	if !ok {
		return OmnibusOrder{}, ErrOrderNotFound
	}
	return *order, nil
}

// ListAllocations 返回按客户号升序排序的当前有效分配明细。
func (s *OmnibusService) ListAllocations(orderID string) ([]Allocation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.orders[orderID]; !ok {
		return nil, ErrOrderNotFound
	}
	result := make([]Allocation, 0, len(s.allocs[orderID]))
	for clientID, qty := range s.allocs[orderID] {
		result = append(result, Allocation{ClientID: clientID, Quantity: qty})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ClientID < result[j].ClientID })
	return result, nil
}

// GetSnapshot 返回冻结时保存的分配快照。
func (s *OmnibusService) GetSnapshot(orderID string) (*AllocationSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot, ok := s.snapshots[orderID]
	if !ok {
		return nil, ErrOrderNotFrozen
	}
	copied := *snapshot
	copied.Allocations = append([]Allocation(nil), snapshot.Allocations...)
	return &copied, nil
}

// Positions 返回订单确认后生成的客户持仓；未确认或失败订单返回空。
func (s *OmnibusService) Positions(orderID string) []Position {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Position(nil), s.positions[orderID]...)
}

// GapReport 返回汇总订单总量与当前客户分配合计之间的差额。
func (s *OmnibusService) GapReport(orderID string) (GapReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	order, ok := s.orders[orderID]
	if !ok {
		return GapReport{}, ErrOrderNotFound
	}
	var allocated int64
	for _, qty := range s.allocs[orderID] {
		allocated += qty
	}
	return GapReport{
		OrderID:     orderID,
		OrderTotal:  order.Total(),
		Allocated:   allocated,
		Gap:         order.Total() - allocated,
		ClientCount: len(s.allocs[orderID]),
	}, nil
}
