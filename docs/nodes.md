# Nodes

| #   | Name       | Alias                                                              | OS          | Owner   | Type              | Hardware                                                                                                                                         | Location      | Price           | 24/7 |
| --- | ---------- | ------------------------------------------------------------------ | ----------- | ------- | ----------------- | ------------------------------------------------------------------------------------------------------------------------------------------------ | ------------- | --------------- | ---- |
| 1   | fredrir-01 | macie                                                              | MacOS       | fredrir | Macbook Pro, 2026 | M5 pro, 15C CPU, 16C GPU, 24 GB RAM, 1 TB SSD                                                                                                    | Trondheim     |                 |      |
| 2   | fredrir-02 | archie                                                             | Arch Linux  | fredrir | Desktop           | AMD Ryzen 7 9800X3D, RTX 5070 Ti Gigabyte Gaming OC, Corsair 32 GB DDR5 6000 MHZ CL30 RAM, ASUS TUF GAMING B850-PLUS WIFI, Crucial T710 4 TB SSD | Trondheim     |                 |      |
| 3   | fredrir-03 | *Undecided name, this machine is WiP and not yet fully configured* | *undecided* | fredrir | Desktop           | AMD 5900X, ASUS B450 TUF GAMING, KINGSTON NV1 2 TB SSD                                                                                           | Trondheim     |                 |      |
| 4   | fredrir-04 | hetzner-one                                                        | NixOS       | Hetzner | ccx23             | x86, 4 VCPU, 16 GB RAM, 160 GB Disk                                                                                                              | eu-central    | 29.90 € / month | Yes  |
| 5   | fredrir-05 | hetzner-two                                                        | NixOS       | Hetzner | CPX22             | x86, 2 VCPU, 4 GB RAM, 80 GB Disk                                                                                                                | eu-central    | 9.99 € / month  | Yes  |
| 6   | fredrir-06 | linode-one                                                         | Ubuntu      | Linode  | Nanode 1 GB       | x86, 1C CPU, 1 GB RAM, 25 GB Disk                                                                                                                | SE, Stockholm | 6.25 $ / month  | Yes  |
| 7 | fredrir-07 | | Ubuntu 26.04.1 LTS | Hetzner | CX33 | x86, 4 vCPU, 8 GB RAM, 80 GB disk | hel1 | | Yes |
| 8 | fredrir-08 | | Ubuntu 26.04.1 LTS | Hetzner | CX33 | x86, 4 vCPU, 8 GB RAM, 80 GB disk | hel1 | | Yes |
| 9 | fredrir-09 | | Ubuntu 26.04 LTS | one.com | Cloud server XXL | x86, 16 vCPU, 32 GB RAM, 800 GB disk | Unverified | | Yes |

## Roles

| Node | Role | Hosting availability |
| --- | --- | --- |
| `fredrir-01` / macie | Administration and development | No hosting dependency |
| `fredrir-02` / archie | Administration and development | No hosting dependency |
| `fredrir-03` | Future hosting capacity | Configuration incomplete |
| `fredrir-04` / hetzner-one | VPS hosting | 24/7 |
| `fredrir-05` / hetzner-two | VPS hosting | 24/7 |
| `fredrir-06` / linode-one | VPS; independent monitoring candidate | 24/7 |
| `fredrir-07`, `fredrir-08` | K3s control-plane candidates | SSH and hardware verified; platform enrollment pending |
| `fredrir-09` | Shared production and CI worker candidate | SSH and hardware verified; platform enrollment pending |
| `fredrir-10` | Reserved for the planned fourth home computer | Hardware and purchase undecided; not provisioned |

| Capacity requirement | Status |
| --- | --- |
| Three K3s control-plane servers | Accepted: two provisioned CX33 plus existing CPX22; enrollment and evacuation pending |
| Shared production and CI workers | Existing CCX23, provisioned one.com XXL and proposed ARM HidenCloud SAR-Torrent; sandbox and capacity gates |
| Independent monitoring | Existing Linode; outside K3s |
| VPS total | Six provisioned; seven if the HidenCloud purchase proceeds |

## Additional capacity

| Quantity | Provider | Model | Architecture | RAM each | Node number | Status |
| --- | --- | --- | --- | --- | --- | --- |
| 2 | Hetzner | CX33 | x86_64 | 8 GB | `fredrir-07`, `fredrir-08` | Provisioned; access and hardware verified |
| 1 | one.com | Cloud server XXL | x86_64 | 32 GB | `fredrir-09` | Provisioned; access and hardware verified |
| 1 | HidenCloud | SAR-Torrent | ARM64 | 32 GB | Unassigned | Unpurchased; provider clarification pending |

See [fleet research](research/fleet-research.md) and [platform implementation](platform.md).
