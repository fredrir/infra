# Nodes

[Production inventory](../ansible/inventory/production.yml) owns host addresses, groups, machine types and resource reservations. [Workload placement](../platform) owns Kubernetes scheduling.

| Node | Alias | Machine | Role |
| --- | --- | --- | --- |
| `fredrir-01` | macie | Macbook Pro, 2026; M5 pro, 15C CPU, 16C GPU, 24 GB RAM, 1 TB SSD; MacOS | Admin |
| `fredrir-02` | archie | Desktop; AMD Ryzen 7 9800X3D, RTX 5070 Ti Gigabyte Gaming OC, Corsair 32 GB DDR5 6000 MHZ CL30 RAM, ASUS TUF GAMING B850-PLUS WIFI, Crucial T710 4 TB SSD; Arch Linux | Admin |
| `fredrir-03` | *undecided* | Desktop; AMD 5900X, ASUS B450 TUF GAMING, KINGSTON NV1 2 TB SSD; *undecided* | *undecided* |
| `fredrir-04` | hetzner-one | ccx23 | Shared production and CI worker |
| `fredrir-05` | hetzner-two | CPX22 | K3s control plane and etcd |
| `fredrir-06` | linode-one | Nanode 1 GB | Independent Gatus monitoring; verification timer |
| `fredrir-07` | hetzner-three | CX33 | K3s control plane and etcd |
| `fredrir-08` | hetzner-four | CX33 | K3s control plane and etcd |
| `fredrir-09` | one-cloud-one | Cloud server XXL | Shared production and CI worker; retained local data |
| `fredrir-10` |  | sx3.16c64r | Tainted K3s agent |
| `fredrir-11` | | Hetzner CX33 | Reconciler; separate provider project |

## Roles

| Capacity         | Value                                                                                         |
| ---------------- | --------------------------------------------------------------------------------------------- |
| Control plane    | Three servers in `hel1`, private etcd network and spread placement group                      |
| Shared workers   | CCX23 and one.com XXL; Kubernetes capability-based placement                                  |
| Volatile worker  | `fredrir-10`; NTNU egress relay; see [volatile workers](runbook.md#volatile-workers)   |
| Inventory        | [Ansible production inventory](../ansible/inventory/production.yml)                           |

See [platform operation](platform.md) and [reconciler operation](runbook.md#reconciler-host).
