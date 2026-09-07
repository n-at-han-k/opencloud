Drop `*.patch` files here. Every one of them is applied to the opencloud source
in the **patched** VM, and none of them in the unpatched one. That is the only
difference between the two.

The unpatched VM is the control: the package exactly as this fleet deploys it.

    git -C <a checkout of v7.5.0> diff > repro/patches/candidate.patch
