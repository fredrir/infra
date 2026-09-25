# Nodes

| #   | Name       | Alias         | OS                 | Owner   | Type              | Hardware                                                                                                                                         | Location      | Price           | 24/7 |
| --- | ---------- | ------------- | ------------------ | ------- | ----------------- | ------------------------------------------------------------------------------------------------------------------------------------------------ | ------------- | --------------- | ---- |
| 1   | fredrir-01 | macie         | MacOS              | fredrir | Macbook Pro, 2026 | M5 pro, 15C CPU, 16C GPU, 24 GB RAM, 1 TB SSD                                                                                                    | Trondheim     |                 |      |
| 2   | fredrir-02 | archie        | Arch Linux         | fredrir | Desktop           | AMD Ryzen 7 9800X3D, RTX 5070 Ti Gigabyte Gaming OC, Corsair 32 GB DDR5 6000 MHZ CL30 RAM, ASUS TUF GAMING B850-PLUS WIFI, Crucial T710 4 TB SSD | Trondheim     |                 |      |
| 3   | fredrir-03 | *undecided*   | *undecided*        | fredrir | Desktop           | AMD 5900X, ASUS B450 TUF GAMING, KINGSTON NV1 2 TB SSD                                                                                           | Trondheim     |                 |      |
| 4   | fredrir-04 | hetzner-one   | Ubuntu 26.04 LTS   | Hetzner | ccx23             | x86, 4 VCPU, 16 GB RAM, 160 GB Disk                                                                                                              | eu-central    | 29.90 € / month | Yes  |
| 5   | fredrir-05 | hetzner-two   | Ubuntu 26.04 LTS   | Hetzner | CPX22             | x86, 2 VCPU, 4 GB RAM, 80 GB Disk                                                                                                                | eu-central    | 9.99 € / month  | Yes  |
| 6   | fredrir-06 | linode-one    | Ubuntu 26.04 LTS   | Linode  | Nanode 1 GB       | x86, 1C CPU, 1 GB RAM, 25 GB Disk                                                                                                                | SE, Stockholm | 6.25 $ / month  | Yes  |
| 7   | fredrir-07 | hetzner-three | Ubuntu 26.04.1 LTS | Hetzner | CX33              | x86, 4 vCPU, 8 GB RAM, 80 GB disk                                                                                                                | hel1          |                 | Yes  |
| 8   | fredrir-08 | hetzner-four  | Ubuntu 26.04.1 LTS | Hetzner | CX33              | x86, 4 vCPU, 8 GB RAM, 80 GB disk                                                                                                                | hel1          |                 | Yes  |
| 9   | fredrir-09 | one-cloud-one | Ubuntu 26.04 LTS   | one.com | Cloud server XXL  | x86, 16 vCPU, 32 GB RAM, 800 GB disk                                                                                                             |               |                 | Yes  |

## Roles

| Node                  | Role                                                 | Availability                    |
| --------------------- | ---------------------------------------------------- | ------------------------------- |
| `fredrir-01` / Macie  | Administration and recovery                          | No hosting dependency           |
| `fredrir-02` / Archie | Administration and recovery                          | No hosting dependency           |
| `fredrir-03`          | Future home capacity                                 | Configuration incomplete        |
| `fredrir-04`          | Shared production and CI worker                      | 24/7                            |
| `fredrir-05`          | K3s control plane and etcd                           | 24/7                            |
| `fredrir-06`          | Independent Gatus monitoring; verification timer     | 24/7; outside Kubernetes        |
| `fredrir-07`          | K3s control plane and etcd                           | 24/7                            |
| `fredrir-08`          | K3s control plane and etcd                           | 24/7                            |
| `fredrir-09`          | Shared production and CI worker; retained local data | 24/7                            |
| `fredrir-10`          | Reserved fourth home computer                        | Hardware and purchase undecided |

| Capacity         | Value                                                                                         |
| ---------------- | --------------------------------------------------------------------------------------------- |
| Provisioned VPSs | Six                                                                                           |
| Control plane    | Three servers in `hel1`, private etcd network and spread placement group                      |
| Shared workers   | CCX23 and one.com XXL; Kubernetes capability-based placement                                  |
| Additional ARM   | HidenCloud SAR-Torrent, unpurchased; provider clarification and runtime qualification pending |
| ARM node number  | Unassigned                                                                                    |
| Inventory        | [Ansible production inventory](../ansible/inventory/production.yml)                           |

See [platform operation](platform.md) and [provider research](research/fleet-research.md).
