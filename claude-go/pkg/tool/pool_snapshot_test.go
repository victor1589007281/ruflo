// pool_snapshot_test.go —— 13.7.9 池观测单测: Pool.Snapshot / 包级登记 / nil 安全。
//
// 锁定三组语义:
//  1. Snapshot 只读镜像登记序 (loaded/resident 过滤, always 排序, enabled 判据
//     = 有成员) 与 nil 池 Enabled=false;
//  2. 包级登记 ObservePool/ObservedPool: nil 拒存, 后装配覆盖前装配;
//  3. Snapshot 是拷贝不是活引用 —— 取快照后继续 Load 不影响已取结果。
package tool

import (
	"reflect"
	"testing"
)

func TestPoolSnapshot(t *testing.T) {
	// nil 池: Enabled=false 零值, 不 panic (心跳路径安全)。
	var nilP *Pool
	s := nilP.Snapshot()
	if s.Enabled || s.Members != 0 {
		t.Errorf("nil pool snapshot = %+v, want zero value", s)
	}

	p := NewPool()
	if got := p.Snapshot(); got.Enabled {
		t.Error("无成员池 Enabled 必须 false (enabled 判据 = 有成员)")
	}

	p.AddMember("beta")
	p.AddMember("alpha")
	p.Load("alpha")
	p.SetResident([]string{"beta"})
	p.SetAlways("pool_search", "pool_load", "pool_release")

	got := p.Snapshot()
	if !got.Enabled {
		t.Error("有成员池 Enabled 必须 true")
	}
	if got.Members != 2 {
		t.Errorf("Members=%d, want 2", got.Members)
	}
	if !reflect.DeepEqual(got.Loaded, []string{"alpha"}) {
		t.Errorf("Loaded=%v, want [alpha]", got.Loaded)
	}
	if !reflect.DeepEqual(got.Resident, []string{"beta"}) {
		t.Errorf("Resident=%v, want [beta]", got.Resident)
	}
	// always 来自 map, 顺序不保证 —— 断言集合与排序。
	if len(got.Always) != 3 {
		t.Fatalf("Always=%v, want 3 件包装方法", got.Always)
	}
	for i := 1; i < len(got.Always); i++ {
		if got.Always[i-1] > got.Always[i] {
			t.Errorf("Always 未排序: %v", got.Always)
			break
		}
	}
	if got.HaveAllowed {
		t.Error("未设白名单 HaveAllowed 必须 false")
	}
	p.SetGovernance(map[string]bool{"x": true}, nil)
	if p.Snapshot().HaveAllowed {
		t.Error("allowed=nil 的治理集 HaveAllowed 仍须 false")
	}
	p.SetGovernance(nil, map[string]bool{"alpha": true})
	if !p.Snapshot().HaveAllowed {
		t.Error("设了 allowed 白名单后 HaveAllowed 必须 true")
	}
}

// TestPoolSnapshotCopy 快照是拷贝: 取快照后再 Load/Release 不影响已取结果。
func TestPoolSnapshotCopy(t *testing.T) {
	p := NewPool()
	p.AddMember("a")
	p.AddMember("b")
	s1 := p.Snapshot()
	if len(s1.Loaded) != 0 {
		t.Fatalf("前置: 应无已加载, got %v", s1.Loaded)
	}
	p.Load("a")
	if len(s1.Loaded) != 0 {
		t.Error("快照被后续 Load 污染 —— Snapshot 必须返回拷贝")
	}
}

// TestObservedPoolRegistry 包级最近池登记: nil 拒存 / 覆盖语义 / 未登记 nil。
func TestObservedPoolRegistry(t *testing.T) {
	if ObservedPool() != nil {
		t.Skip("包级状态已被其他测试装配 (RegisterPoolTools 顺序), 覆盖断言改为局部")
	}
	// nil 拒存: 登记后仍为 nil。
	ObservePool(nil)
	if ObservedPool() != nil {
		t.Error("ObservePool(nil) 不得覆盖登记")
	}
	p1 := NewPool()
	p1.AddMember("x")
	ObservePool(p1)
	if ObservedPool() != p1 {
		t.Error("登记后必须取回同实例")
	}
	p2 := NewPool()
	p2.AddMember("y")
	ObservePool(p2)
	if ObservedPool() != p2 {
		t.Error("后装配必须覆盖前装配 (最近池语义)")
	}
}
