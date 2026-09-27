package policy

import (
	"reflect"
	"testing"
)

func TestGrafanaNeverRunsTwoPodsOnItsDatabase(t *testing.T) {
	t.Parallel()
	strategy := at(load(t, "platform/components/observability/monitoring.yaml"), "spec", "values", "grafana", "deploymentStrategy")
	want := object{"type": "RollingUpdate", "rollingUpdate": object{"maxSurge": 0, "maxUnavailable": 1}}
	if !reflect.DeepEqual(strategy, want) {
		t.Errorf("Grafana deployment strategy %v, want %v", strategy, want)
	}
}
