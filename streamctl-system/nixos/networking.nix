{ lib, ... }: {
  # This file was populated at runtime with the networking
  # details gathered from the active system.
  networking = {
    nameservers = [ "8.8.8.8"
 ];
    defaultGateway = "134.122.0.1";
    defaultGateway6 = {
      address = "2604:a880:800:14::1";
      interface = "eth0";
    };
    dhcpcd.enable = false;
    usePredictableInterfaceNames = lib.mkForce false;
    interfaces = {
      eth0 = {
        ipv4.addresses = [
          { address="134.122.0.29"; prefixLength=20; }
{ address="10.17.0.5"; prefixLength=16; }
        ];
        ipv6.addresses = [
          { address="2604:a880:800:14:0:3:af2:f000"; prefixLength=64; }
{ address="fe80::cb5:27ff:feff:72ac"; prefixLength=64; }
        ];
        ipv4.routes = [ { address = "134.122.0.1"; prefixLength = 32; } ];
        ipv6.routes = [ { address = "2604:a880:800:14::1"; prefixLength = 128; } ];
      };
            eth1 = {
        ipv4.addresses = [
          { address="10.108.0.4"; prefixLength=20; }
        ];
        ipv6.addresses = [
          { address="fe80::5c53:a8ff:fe2d:1eaf"; prefixLength=64; }
        ];
        };
    };
  };
  services.udev.extraRules = ''
    ATTR{address}=="0e:b5:27:ff:72:ac", NAME="eth0"
    ATTR{address}=="5e:53:a8:2d:1e:af", NAME="eth1"
  '';
}
