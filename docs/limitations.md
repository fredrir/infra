# Limitations

- L1. CI gate red-blocks a deliberately broken flake on a test PR; green on main.

- L2. Grafana (tailnet) shows live dashboards for both hosts + the backend; Loki answers a cross-host journal query; one alert fires end-to-end on a forced condition (e.g. stopped unit) and reaches the owner; **the public-path blackbox probe is green through the CF edge; stopping the observability stack notifies the owner within 15 minutes via the external dead-man** (second-opinion additions — the alerting-is-dark case and the public path were previously unwatched).


- L3. `llunde.no`/`www`/`api` serve through the tunnel with the XFF/rate-limit verification green; `ss` and the Hetzner firewall show **zero public inbound on both hosts**; origin A/AAAA records gone from DNS; break-glass procedure written and its tofu diffs pre-staged.

- L4. A trivial infra merge applies itself to both hosts with zero laptop involvement; a deliberately failing apply stops the sequence and alerts; the manual path demonstrated still working afterward.

- L5. tofu plan clean; flake goldens extended where load-bearing (cloudflared unit, observability units).

- L6. Owner review → gate closed.