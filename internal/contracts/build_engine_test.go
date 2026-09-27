package contracts

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildEngineMountsItsCachePolicyWhereTheEngineReadsIt(t *testing.T) {
	unit := string(read(t, filepath.Join(root(t), "ansible/roles/build_engine/templates/infra-dagger.service.j2")))
	if mount := "--volume /etc/infra-dagger.toml:/etc/dagger/engine.toml:ro "; !strings.Contains(unit, mount) {
		t.Fatalf("build engine unit does not mount its cache policy with %q, the configuration the pinned engine entrypoint passes to dagger-engine:\n%s", mount, unit)
	}
}
