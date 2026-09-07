{
  description = "Two VMs running opencloud from source, one unpatched and one patched, both profiled";

  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs/nixos-26.05";
    microvm = {
      url = "github:microvm-nix/microvm.nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs = { self, nixpkgs, microvm }:
    let
      system = "x86_64-linux";
      pkgs = nixpkgs.legacyPackages.${system};

      # Anything dropped into ./patches is applied in the patched VM and in
      # neither the unpatched one. That is the ONLY difference between them.
      candidatePatches =
        let dir = ./patches; in
        if builtins.pathExists dir then
          map (f: dir + "/${f}")
            (builtins.filter (f: nixpkgs.lib.hasSuffix ".patch" f)
              (builtins.attrNames (builtins.readDir dir)))
        else [ ];

      # The real server. ./pkgs/opencloud is the package definition this fleet
      # already deploys, copied verbatim: it is the build that produced the
      # binary the problem was seen on, source and all. It is NOT rebuilt from a
      # local checkout -- doing that changes the thing under test.
      mkOpencloud = { name, patches ? [ ] }:
        let base = pkgs.callPackage ./pkgs/opencloud/package.nix { }; in
        base.overrideAttrs (old: {
          pname = "opencloud-${name}";
          patches = (old.patches or [ ]) ++ patches;
          # The build is the control; running the suite is not what this is for
          # and takes far longer than the measurement.
          doCheck = false;
          doInstallCheck = false;
        });

      # memMB: NOT 2048. QEMU hangs on exactly 2 GB
      # (microvm-nix/microvm.nix#171) and microvm.nix warns at eval time. Lower
      # it to make the server meet the ceiling sooner.
      mkVM = { name, patches ? [ ], memMB ? 3072, files ? 20000 }:
        let
          opencloud = mkOpencloud { inherit name patches; };
        in
        nixpkgs.lib.nixosSystem {
          inherit system;
          modules = [
            microvm.nixosModules.microvm
            {
              networking.hostName = "oc-${name}";
              system.stateVersion = "24.11";

              microvm = {
                hypervisor = "qemu";
                # No swap, as on the machine this was seen on: the kernel either
                # satisfies an allocation or kills the process.
                mem = memMB;
                vcpu = 4;
                shares = [{
                  tag = "repro-out";
                  source = "./repro-out";
                  mountPoint = "/repro-out";
                  # 9p, not virtiofs: virtiofs needs a virtiofsd daemon started
                  # alongside the VM, and the point is that `nix run` is the
                  # whole procedure.
                  proto = "9p";
                }];
              };

              services.opencloud = {
                enable = true;
                package = opencloud;
                address = "127.0.0.1";
                port = 9200;
                url = "http://127.0.0.1:9200";

                environment = {
                  OC_INSECURE = "true";
                  OC_LOG_LEVEL = "warn";

                  # Basic auth, so the server can be driven over WebDAV without going
                  # through an OIDC flow.
                  PROXY_ENABLE_BASIC_AUTH = "true";

                  # THE PROFILING. Per service, and there is no global form: the
                  # Pprof field carries only its own name, unlike LogLevel which
                  # also accepts OC_LOG_LEVEL. OC_DEBUG_PPROF does not exist.
                  SEARCH_DEBUG_PPROF = "true";
                  SEARCH_DEBUG_ADDR = "127.0.0.1:9224";
                  STORAGE_USERS_DEBUG_PPROF = "true";
                  STORAGE_USERS_DEBUG_ADDR = "127.0.0.1:9159";
                };
              };

              # No MemoryMax: it should meet the machine's ceiling the way the
              # real one did, not a cgroup's. No restart either, so a kill stays
              # visible instead of being papered over.
              systemd.services.opencloud.serviceConfig = {
                MemoryAccounting = true;
                Restart = nixpkgs.lib.mkForce "no";
              };

              services.getty.autologinUser = "root";
              users.users.root.password = "";
            }
          ];
        };

      vms = {
        vm-unpatched = mkVM { name = "unpatched"; };
        vm-patched = mkVM { name = "patched"; patches = candidatePatches; };
      };

      runnerApp = vm: {
        type = "app";
        program = "${vm.config.microvm.declaredRunner}/bin/microvm-run";
      };
    in
    {
      nixosConfigurations = vms;

      apps.${system} = builtins.mapAttrs (_: runnerApp) vms;

      packages.${system} = {
        opencloud-unpatched = mkOpencloud { name = "unpatched"; };
        opencloud-patched = mkOpencloud { name = "patched"; patches = candidatePatches; };
      };
    };
}
