{lib}: let
  renderValue = v:
    if lib.isBool v
    then lib.boolToString v
    else toString v;

  renderEntry = key: value:
    if lib.isList value
    then map (item: "${key}=${renderValue item}") value
    else ["${key}=${renderValue value}"];

  renderSection = name: attrs:
    lib.optionals (attrs != {}) [
      ("[${name}]\n" + lib.concatStringsSep "\n" (lib.flatten (lib.mapAttrsToList renderEntry attrs)) + "\n")
    ];

  renderUnitFile = sections:
    lib.concatStringsSep "\n" (lib.flatten (map ({
      name,
      attrs,
    }:
      renderSection name attrs)
    sections));

  etcFragment = uid: fileName: text: {
    "containers/systemd/users/${toString uid}/${fileName}" = {
      inherit text;
      mode = "0644";
    };
  };
in {
  mkContainerUnit = {
    name,
    uid,
    container,
    unit ? {},
    service ? {},
    install ? {},
  }:
    etcFragment uid "${name}.container" (renderUnitFile [
      {
        name = "Unit";
        attrs = unit;
      }
      {
        name = "Container";
        attrs = container;
      }
      {
        name = "Service";
        attrs = service;
      }
      {
        name = "Install";
        attrs = install;
      }
    ]);

  mkNetworkUnit = {
    name,
    uid,
    network ? {},
    unit ? {},
    install ? {},
  }:
    etcFragment uid "${name}.network" (renderUnitFile [
      {
        name = "Unit";
        attrs = unit;
      }
      {
        name = "Network";
        attrs = network;
      }
      {
        name = "Install";
        attrs = install;
      }
    ]);
}
