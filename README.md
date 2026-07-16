# ossein-kernel

This project is a linux 7.x _kernel image builder for macOS_.

It does provide a baseline configuration and patches that are specifically
tailored for use in the context of Apple VZ, focused on performance and minimal footprint.

As such, the primary intended consumer of these kernels is [Ossein](https://github.com/farcloser/ossein),
though it is not limited to this, as users can point to their own configuration, and elect to different
patches to build any kernel as they see fit.

The baseline setup produces kernel images under 10MB, that are outperforming or
tie-ing kernels from other macOS container runtimes.

## How to build it

### Requirements

* macOS 26+
* aqua installed

```
TBD
```

## Layout

Stock kernel configuration fragments live under kernel/config.

Patches live under kernel/patches.

The kernel builder logic is in cmd/ossein-kernel/main.go, and the actual in-container
build script is in cmd/ossein-kernel/build.sh
