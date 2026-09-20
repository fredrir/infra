package kata

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestDoctorRejectsUnboundedOrOversizedResources(t *testing.T) {
	for _, tc := range []struct{ name, value string }{{"cpu.max", "max 100000"}, {"cpu.max", "300000 100000"}, {"memory.max", "max"}, {"memory.max", "8589934592"}, {"pids.max", "512"}, {"memory.swap.max", "1"}, {"cpu.max", "-1 100000"}} {
		t.Run(tc.name+tc.value, func(t *testing.T) {
			root := t.TempDir()
			for name, value := range map[string]string{"cpu.max": "200000 100000", "memory.max": "4294967296", "pids.max": "256", "memory.swap.max": "0"} {
				if e := os.WriteFile(filepath.Join(root, name), []byte(value), 0644); e != nil {
					t.Fatal(e)
				}
			}
			if _, e := doctorCgroup(root, DefaultLimits(), DoctorReport{}); e != nil {
				t.Fatal(e)
			}
			if e := os.WriteFile(filepath.Join(root, tc.name), []byte(tc.value), 0644); e != nil {
				t.Fatal(e)
			}
			if _, e := doctorCgroup(root, DefaultLimits(), DoctorReport{}); e == nil {
				t.Fatal("unsafe resource limit accepted")
			}
		})
	}
}

func TestPackageInventoryRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	if e := os.WriteFile(filepath.Join(root, "package.list"), []byte("/etc/../../escape\n"), 0644); e != nil {
		t.Fatal(e)
	}
	if e := packageFiles(root, filepath.Join(root, "out.json")); e == nil {
		t.Fatal("package traversal accepted")
	}
}
func TestGuestRequiresExplicitIsolationChoice(t *testing.T) {
	e := Build(context.Background(), BuildOptions{Component: "guest", Limits: DefaultLimits(), Log: io.Discard}, DaggerExecutor{})
	if e == nil {
		t.Fatal("guest accepted without isolated engine")
	}
}
func TestNativeQualificationRequiresExplicitDisposableHost(t *testing.T) {
	if _, e := Qualify(context.Background(), QualifyOptions{}); e == nil {
		t.Fatal("native qualification accepted without disposable host")
	}
}

func TestEngineContractRejectsMissingAndExcessiveLimits(t *testing.T) {
	valid := engineLimits{NanoCPUs: 2e9, Memory: 4 << 30, MemorySwap: 4 << 30, PidsLimit: 256}
	if e := valid.validate(DefaultLimits()); e != nil {
		t.Fatal(e)
	}
	for _, change := range []func(*engineLimits){func(v *engineLimits) { v.NanoCPUs = 0 }, func(v *engineLimits) { v.NanoCPUs = 3e9 }, func(v *engineLimits) { v.Memory = 0 }, func(v *engineLimits) { v.MemorySwap = -1 }, func(v *engineLimits) { v.PidsLimit = 0 }, func(v *engineLimits) { v.PidsLimit = 512 }} {
		v := valid
		change(&v)
		if e := v.validate(DefaultLimits()); e == nil {
			t.Fatalf("invalid engine accepted: %+v", v)
		}
	}
}
