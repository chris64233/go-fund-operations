package fundoperations

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
)

// State 服务的全部持久化状态：基金、申请、批次快照与净值版本。
type State struct {
	Funds        map[string]*Fund                   `json:"funds"`
	Applications map[string]map[string]*Application `json:"applications"` // fundID -> externalID -> 申请
	Batches      map[string]map[string]*Batch       `json:"batches"`      // fundID -> 估值日 -> 批次快照
	NAVs         map[string]map[string][]NAV        `json:"navs"`         // fundID -> 日期 -> 净值版本（升序）
}

func newState() *State {
	return &State{
		Funds:        make(map[string]*Fund),
		Applications: make(map[string]map[string]*Application),
		Batches:      make(map[string]map[string]*Batch),
		NAVs:         make(map[string]map[string][]NAV),
	}
}

// normalize 保证反序列化后所有 map 非空。
func (s *State) normalize() {
	if s.Funds == nil {
		s.Funds = make(map[string]*Fund)
	}
	if s.Applications == nil {
		s.Applications = make(map[string]map[string]*Application)
	}
	if s.Batches == nil {
		s.Batches = make(map[string]map[string]*Batch)
	}
	if s.NAVs == nil {
		s.NAVs = make(map[string]map[string][]NAV)
	}
}

// Persister 状态持久化接口，每次状态变更后由 Service 调用。
type Persister interface {
	Save(state *State) error
}

// NoopPersister 不持久化，仅内存态，适用于测试。
type NoopPersister struct{}

// Save 不做任何事。
func (NoopPersister) Save(*State) error { return nil }

// FilePersister 将状态以 JSON 快照形式持久化到文件（临时文件 + 原子改名）。
type FilePersister struct {
	Path string
}

// NewFilePersister 构造文件持久化器。
func NewFilePersister(path string) FilePersister { return FilePersister{Path: path} }

// Save 将状态写入文件。
func (p FilePersister) Save(state *State) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := p.Path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p.Path)
}

// LoadState 从 JSON 文件加载状态。
func LoadState(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	state.normalize()
	return &state, nil
}

// NewFileService 构造以 JSON 文件持久化的 Service；文件已存在时加载其状态。
func NewFileService(path string, clock Clock) (*Service, error) {
	state, err := LoadState(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		state = newState()
	}
	return newService(state, clock, NewFilePersister(path)), nil
}
