package contracts

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildEngineMountsItsCachePolicyWhereTheEngineReadsIt(t *testing.T) {
	unit := string(read(t, filepath.Join(root(t), "ansible/roles/build_engine/templates/infra-dagger.service.j2")))
	for _, mount := range []string{"--volume /etc/infra-dagger/{{ build_engine.slug }}.toml:/etc/dagger/engine.toml:ro ", "--volume /etc/infra-dagger/engine.json:/etc/dagger/engine.json:ro "} {
		if !strings.Contains(unit, mount) {
			t.Fatalf("build engine unit does not mount %q, where the pinned engine reads its configuration:\n%s", mount, unit)
		}
	}
}
