package fslock

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTryLockFailsWhileHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	open := func() *os.File {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		return f
	}
	unlock, err := Lock(open())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := TryLock(open()); ok || err != nil {
		t.Fatalf("TryLock while held: ok=%v err=%v", ok, err)
	}
	unlock()
	u2, ok, err := TryLock(open())
	if !ok || err != nil {
		t.Fatalf("TryLock after release: ok=%v err=%v", ok, err)
	}
	u2()
}
