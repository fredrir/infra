package contracts

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/fredrir/infra/internal/dev"
)

func TestBuildEngineMountsItsCachePolicyWhereTheEngineReadsIt(t *testing.T) {
	unit := string(read(t, filepath.Join(root(t), "ansible/roles/build_engine/templates/infra-dagger.service.j2")))
	if mount := "--volume /etc/infra-dagger.toml:" + dev.EngineConfigPath + ":ro "; !strings.Contains(unit, mount) {
		t.Fatalf("build engine unit does not mount its cache policy with %q:\n%s", mount, unit)
	}
}
