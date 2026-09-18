package pool

import (
	"path/filepath"
	"testing"
	"time"

	"qoderwork2api/internal/cred"
)

func TestPickHighestCredits(t *testing.T) {
	p := New("")
	p.Add(&cred.Cred{UID: "u1", DT: "dt-1"})
	p.Add(&cred.Cred{UID: "u2", DT: "dt-2"})
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 500)
	got := p.Pick()
	if got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v", got)
	}
}

func TestPickSkipsCooling(t *testing.T) {
	p := New("")
	p.Add(&cred.Cred{UID: "u1", DT: "dt-1"})
	p.Add(&cred.Cred{UID: "u2", DT: "dt-2"})
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 50)
	p.Cooldown("u1", CoolHard, time.Hour, "test")
	if got := p.Pick(); got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v", got)
	}
}

func TestPickExpiredCooldownReturns(t *testing.T) {
	p := New("")
	p.Add(&cred.Cred{UID: "u1", DT: "dt-1"})
	p.SetCredits("u1", 100)
	p.Cooldown("u1", CoolSoft, time.Millisecond, "429")
	time.Sleep(5 * time.Millisecond)
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("pick=%+v", got)
	}
}

func TestPickNilWhenAllCooling(t *testing.T) {
	p := New("")
	p.Add(&cred.Cred{UID: "u1", DT: "dt-1"})
	p.Cooldown("u1", CoolHard, time.Hour, "x")
	if p.Pick() != nil {
		t.Fatal("want nil")
	}
}

func TestPickExcluding(t *testing.T) {
	p := New("")
	p.Add(&cred.Cred{UID: "u1", DT: "dt-1"})
	p.Add(&cred.Cred{UID: "u2", DT: "dt-2"})
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 50)
	tried := map[string]bool{"u1": true}
	got := p.PickExcluding(tried)
	if got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v", got)
	}
	tried["u2"] = true
	if got := p.PickExcluding(tried); got != nil {
		t.Fatalf("want nil")
	}
}

func TestCooldownPersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&cred.Cred{UID: "u1", DT: "dt-1"})
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	p2 := New(fp)
	p2.Add(&cred.Cred{UID: "u1", DT: "dt-1"})
	if p2.Pick() != nil {
		t.Fatal("cooldown lost after reload")
	}
}

func TestDisablePersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&cred.Cred{UID: "u1", DT: "dt-1"})
	p.Disable("u1", "auth invalid")
	p2 := New(fp)
	p2.Add(&cred.Cred{UID: "u1", DT: "dt-1"})
	if p2.Pick() != nil {
		t.Fatal("disabled picked after reload")
	}
	st, _ := p2.Status("u1")
	if !st.Disabled || st.Reason != "auth invalid" {
		t.Errorf("st=%+v", st)
	}
}

func TestReenableIfCredits(t *testing.T) {
	p := New("")
	p.Add(&cred.Cred{UID: "u1", DT: "dt-1"})
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	p.ReenableIfCredits("u1", 500)
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("should reenable")
	}
}

func TestReenableZeroCreditsStays(t *testing.T) {
	p := New("")
	p.Add(&cred.Cred{UID: "u1", DT: "dt-1"})
	p.Cooldown("u1", CoolHard, time.Hour, "x")
	p.ReenableIfCredits("u1", 0)
	if p.Pick() != nil {
		t.Fatal("zero credits should stay cooling")
	}
}

func TestReenableDoesNotTouchDisabled(t *testing.T) {
	p := New("")
	p.Add(&cred.Cred{UID: "u1", DT: "dt-1"})
	p.Disable("u1", "auth invalid")
	p.ReenableIfCredits("u1", 500)
	if p.Pick() != nil {
		t.Fatal("disabled must not auto-reenable")
	}
}

func TestNoteErrorThreshold(t *testing.T) {
	p := New("")
	p.Add(&cred.Cred{UID: "u1", DT: "dt-1"})
	for i := 0; i < 2; i++ {
		p.NoteError("u1", 3, time.Hour)
		if p.Pick() == nil {
			t.Fatalf("cooling too early at %d", i+1)
		}
	}
	p.NoteError("u1", 3, time.Hour)
	if p.Pick() != nil {
		t.Fatal("threshold 3 should cool")
	}
}

func TestNoteSuccessResets(t *testing.T) {
	p := New("")
	p.Add(&cred.Cred{UID: "u1", DT: "dt-1"})
	p.NoteError("u1", 3, time.Hour)
	p.NoteError("u1", 3, time.Hour)
	p.NoteSuccess("u1")
	p.NoteError("u1", 3, time.Hour)
	p.NoteError("u1", 3, time.Hour)
	if p.Pick() == nil {
		t.Fatal("success should reset counter")
	}
}

func TestList(t *testing.T) {
	p := New("")
	p.Add(&cred.Cred{UID: "u1", DT: "dt-1", Nickname: "nick"})
	p.Add(&cred.Cred{UID: "u2", DT: "dt-2"})
	p.SetCredits("u1", 42)
	p.Cooldown("u2", CoolSoft, time.Minute, "429")
	list := p.List()
	if len(list) != 2 {
		t.Fatalf("list=%d", len(list))
	}
	var s1, s2 Status
	for _, s := range list {
		if s.UID == "u1" {
			s1 = s
		}
		if s.UID == "u2" {
			s2 = s
		}
	}
	if s1.Credits != 42 || s1.Nickname != "nick" || s1.Cooling || s1.Disabled {
		t.Errorf("s1=%+v", s1)
	}
	if !s2.Cooling || s2.Reason != "429" {
		t.Errorf("s2=%+v", s2)
	}
}

// 机器指纹持久化：重启后 MachineID 不变
func TestMachineFingerprintPersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	c := &cred.Cred{UID: "u1", DT: "dt-1", MachineID: "mid-x", MachineToken: "mt-x", MachineType: "mty-x"}
	p.Add(c)
	p.SaveState()

	// 模拟重启：新建 pool + 加载同 UID 但指纹为空的新 Cred
	p2 := New(fp)
	c2 := &cred.Cred{UID: "u1", DT: "dt-1"}
	p2.Add(c2)
	got := p2.AuthByUID("u1")
	if got.MachineID != "mid-x" || got.MachineToken != "mt-x" || got.MachineType != "mty-x" {
		t.Errorf("fingerprint lost: %+v", got)
	}
}
