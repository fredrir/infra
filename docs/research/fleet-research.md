# Fleet research

| Name | Value |
| --- | --- |
| Verified | 2026-09-12; public provider and upstream documentation |
| Status | CX33 nodes `fredrir-07` and `fredrir-08`, plus one.com XXL `fredrir-09`, provisioned; all unenrolled; HidenCloud unpurchased |
| Topology | Three Hetzner control-plane servers; mixed-provider, mixed-architecture workers |
| VPS count | Seven total: four additions plus all three existing VPSs; six K3s nodes and one external Linode |
| References | [Platform design](k3s-platform.md), [node inventory](../nodes.md), [version and security research](versions-security.md) |

## Provider facts

| Server | Published specification | Verification |
| --- | --- | --- |
| Hetzner CX33 (`fredrir-07`, `fredrir-08`) | Each: 4 shared x86 vCPU, 8 GB RAM, 80 GB NVMe | Read-only API/SSH confirms `hel1`, Ubuntu 26.04.1, no private network or placement group; IDs and observations recorded in [machine inventory](../../platform/inventory/nodes.json). [CX specifications](https://www.hetzner.com/cloud/cost-optimized/) |
| Existing Hetzner CPX22 (`fredrir-05`) | 2 shared x86 vCPU, 4 GB RAM, 80 GB disk | Existing [inventory](../nodes.md); proposed third control-plane server after application and database evacuation. [CPU allocation](https://docs.hetzner.com/cloud/servers/faq/) |
| Existing Hetzner CCX23 (`fredrir-04`) | 4 dedicated x86 vCPU, 16 GB RAM, 160 GB NVMe | Proposed worker; a dedicated vCPU is one physical-core thread, not an entire physical core. [CCX specifications](https://www.hetzner.com/cloud/general-purpose/), [CPU allocation](https://docs.hetzner.com/cloud/servers/faq/) |
| one.com Cloud server XXL (`fredrir-09`) | 16 vCPU, 32 GB RAM, 800 GB NVMe, 1 Gbit/s, unlimited traffic; AMD EPYC advertised | Read-only SSH confirms Ubuntu 26.04, x86, 16 vCPU, 32,087 MiB RAM and 800 GiB disk; transport remains unenrolled. Regional CPU entitlement and IOPS remain unverified. [Unmanaged VPS](https://www.one.com/en-gb/vps/) |
| New HidenCloud EU-VPS SAR-Torrent | ARM Ampere Altra; 16 vCPU, 32 GB RAM, 320 GB NVMe; advertised 10 Gbit/s, 20 TB traffic and two backups | Exact user-supplied [root VPS offer](https://dash.hidencloud.com/store/view/178); product documentation identifies the SAR range as **shared CPU** and the product page specifies **Arm64**. [CPU classification](https://docs.hidencloud.com/vps-paid-plan/vps-servers.md), [architecture](https://www.hidencloud.com/service/vps) |
| Existing Linode (`fredrir-06`) | x86, 1 core, 1 GB RAM, 25 GB disk | Retained for independent monitoring and lightweight jobs; existing [inventory](../nodes.md) |

| Provider capability | Confirmed | Pending |
| --- | --- | --- |
| Hetzner isolation | KVM with virtio disks/network; Cloud does **not** support nested virtualization. [Technical FAQ](https://docs.hetzner.com/cloud/technical-details/faq/), [server FAQ](https://docs.hetzner.com/cloud/servers/faq/) | CX/CPX sustained performance under CPU contention; dedicated control-plane **role** does not make their CPU allocation dedicated |
| Observed CX33 OS | Ubuntu 26.04.1 LTS; Canonical lists standard security maintenance through May 2031. [Lifecycle](https://ubuntu.com/about/release-cycle), [release](https://releases.ubuntu.com/26.04/) | Adapter accepts 26.04 without reinstall; observed kernel 7.0.0-30 requires the same K3s, network and runtime pilot gates |
| one.com isolation | Full root; published resource reservation; own European OpenStack infrastructure. [VPS](https://www.one.com/en-gb/vps/) | Hypervisor, physical CPU exclusivity/overcommit policy, nested virtualization, custom kernel and NixOS boot/recovery support; OpenStack alone establishes none of these |
| one.com storage | 800 GB NVMe for XXL. [VPS](https://www.one.com/en-gb/vps/) | The claimed **5,000 IOPS** was not found in current official English/Norwegian specifications; distinguish guaranteed sustained IOPS from a ceiling |
| one.com containers | Official Ubuntu Docker installation instructions. [Docker guide](https://help.one.com/hc/en-us/articles/38537932780561-Installing-Docker-on-a-one-com-VPS) | Exact K3s, CNI and CI sandbox compatibility on the selected image |
| HidenCloud OS and access | Root VPS; dedicated IPv4 and IPv6; ARM Docker CE and WireGuard images; ARM NixOS 24.05 ISO listed. [VPS documentation](https://docs.hidencloud.com/dashboard/vps) | Current maintained NixOS installation, kernel/module control, hypervisor, nested virtualization, rescue console and reinstall recovery; listed historical ISO is not a supported-release recommendation |
| HidenCloud location and network | Offer requires a location selection; location cannot change after creation; 10 Gbit/s and 20 TB advertised. [Exact offer](https://dash.hidencloud.com/store/view/178) | Available location values, actual datacenter/underlying infrastructure provider, independence from Hetzner, sustained throughput and shared uplink limits |
| HidenCloud permitted use | Checkout-linked terms and advertised images conflict; see the dated review below | Written product-specific clarification before purchase |

## HidenCloud purchase review — 2026-09-12

| Name | Finding |
| --- | --- |
| Exact product | SAR-Torrent is explicitly sold as a **root VPS**, with image/ISO changes offered; this establishes advertised administrative access, not unrestricted usage permission. [Offer](https://dash.hidencloud.com/store/view/178) |
| Technical evidence | ARM Ubuntu 24.04, Docker CE, WireGuard and GitLab images are listed; this supports a Linux/container pilot, but does not verify gVisor, kernel modules or K3s operation. [Root VPS documentation](https://docs.hidencloud.com/dashboard/vps) |
| Governing checkout link | The exact offer's Terms and Conditions link redirects to the current [terms](https://docs.hidencloud.com/legal/terms); no root-VPS networking exception was found there |
| Proxy restriction | §4.1(d) prohibits “any proxy setups or connections”; private overlays, reverse proxies and cloudflared have no stated exemption. §2.5's registration VPN restriction is separate. [Terms](https://docs.hidencloud.com/legal/terms) |
| CI resource policy | §7.3 states a 25% shared-server resource limit without identifying its denominator or SAR applicability; permitted sustained build load remains unclear. [Terms](https://docs.hidencloud.com/legal/terms) |
| Backup discrepancy | Two backups are advertised, while §9 disclaims customer backup services; confirm scope and retention, and retain independent backups. [Offer](https://dash.hidencloud.com/store/view/178), [terms](https://docs.hidencloud.com/legal/terms) |
| Written answer required | Confirm SAR-Torrent permits private Tailscale/WireGuard, K3s CNI, reverse proxy/cloudflared and container CI; clarify §7.3's denominator and sustained CPU allowance |
| Recommendation | Keep HidenCloud unpurchased until those product-specific answers and the kernel/recovery prerequisites are clear; root access and images do not resolve the terms conflict |

## Capacity and failure domains

| Name | Assessment |
| --- | --- |
| Control-plane sizing | Two CX33 servers plus CPX22 give individual capacities of 4 vCPU/8 GB, 4 vCPU/8 GB and 2 vCPU/4 GB; each meets the K3s baseline of 2 CPU/2 GB. A credible small-cluster starting point, conditional on measured etcd disk latency, CPU contention and API responsiveness. [K3s requirements](https://docs.k3s.io/installation/requirements) |
| Control-plane reservations | Keep application builds, databases and bulk telemetry off these servers; reserve host/K3s resources and limit permitted system agents/controllers. Each etcd member stores its own datastore copy; 20 GB aggregate RAM is not a shared datastore memory pool |
| Quorum | Three embedded-etcd servers tolerate one unavailable member; retain two healthy members during maintenance. [K3s HA](https://docs.k3s.io/datastore/ha-embedded) |
| Hetzner placement | Keep server members in `hel1` on private networking; use spread placement across physical hosts. Spread placement does not establish separate sites; adding an existing server to a placement group requires it to be off. [Placement groups](https://docs.hetzner.com/cloud/placement-groups/faq/) |
| Multicloud workers | Distributed agents are supported; embedded-etcd servers should share a location and reach each other privately. Additional latency can affect performance and cluster health. [K3s multicloud](https://docs.k3s.io/networking/distributed-multicloud) |
| API independence | Workers depend on the Hetzner quorum/API for scheduling and reconciliation; another provider's worker is not an independent cluster. Existing containers may continue through an API outage, but deployment, rescheduling and recovery cannot be assumed |
| API bootstrap | Host networking, admin access and the stable API endpoint must work before cluster add-ons; preserve a direct recovery path independent of in-cluster DNS, ingress and Flux |
| Worker totals | Proposed raw capacity: 36 vCPU, 80 GB RAM, 1,280 GB local disk; includes different CPU entitlements and architectures. Subtract OS, runtime, system services, production reservations and recovery headroom before allocating CI |
| one.com worker loss | Remaining raw capacity: 16 GB x86 RAM on CCX23 plus 32 GB ARM RAM; x86-only workloads cannot consume the ARM capacity automatically |
| HidenCloud worker loss | Remaining raw capacity: 48 GB x86 RAM; no native ARM worker remains. Native ARM builds stop unless another eligible ARM worker exists |
| Hetzner site outage | Can remove all control-plane members and the CCX23 worker; the other workers do not supply etcd quorum. HidenCloud's physical failure-domain independence remains unverified |
| Local storage | The 1,280 GB total is not a replicated or interchangeable volume pool; K3s local-path volumes retain node locality. Application backups and restore procedures remain separate from etcd recovery. [K3s storage](https://docs.k3s.io/add-ons/storage), [datastore backup](https://docs.k3s.io/datastore/backup-restore) |
| Shared production/CI | Control-plane separation does not isolate CI from production on a shared worker; admission, sandbox compatibility and aggregate budgets must pass the platform's CI gates. Nested KVM cannot be assumed as a Hetzner fallback |
| Existing Linode and home nodes | `fredrir-06` stays outside quorum for small external checks; admin laptops stay outside hosting. Future home workers add opportunistic capacity without satisfying baseline availability or quorum |

## Procurement and pilot gates

| Gate | Required evidence |
| --- | --- |
| Exact offer | Two **CX33** instances in the chosen Hetzner location; one.com regional **XXL** order details; HidenCloud root **SAR-Torrent** offer and chosen location |
| Provider fit | Written answers for unresolved virtualization, kernel/ISO/recovery, CPU entitlement, IOPS and traffic limits; HidenCloud terms clarification; actual datacenter and underlying provider recorded |
| Bootstrap and recovery | Reproducible maintained OS install, out-of-band recovery, reboot and restore of a disposable node; architecture and virtual hardware verified from the provisioned system |
| Kernel and networking | K3s/containerd, cgroups, required CNI/kernel modules and selected sandbox work on each provider/architecture; test encrypted cross-provider pod traffic, MTU, reconnects and API reachability |
| Control-plane load | Measure etcd write latency, CPU steal, memory and disk growth under reconciliation/CI API load; lose each control-plane member in turn while API and quorum remain healthy |
| Architecture contract | Every production image and required add-on supports each eligible architecture; native build/test both variants before publishing a multiarch index; unsupported binaries fail admission or placement |
| Capacity | Test production plus capped CI, then each worker loss; validate CPU, memory, storage and PID headroom per architecture and volume eligibility; CI queues instead of taking recovery capacity |
| Stateful migration | Restore and verify application data before evacuating CPX22; identify the sole writer during cutover; retain a tested rollback with explicit handling of writes made after cutover |
| Outage behavior | Test API/Hetzner-site loss separately from worker loss; external checks report failure, admin recovery remains reachable, and restoration uses protected datastore+token and separate application backups |
| Purchase status | User provisioned two CX33 and one.com XXL; HidenCloud remains unpurchased; automation performed read-only inspection, no purchase or activation |
