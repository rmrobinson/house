"""Macro for packaging a house go_binary as a multi-arch OCI image and pushing it
to the self-hosted registry. See README.md for the overall approach.
"""

load("@rules_oci//oci:defs.bzl", "oci_image", "oci_image_index", "oci_push")
load("@rules_pkg//pkg:tar.bzl", "pkg_tar")
load(":registry.bzl", "REGISTRY")
load(":transition.bzl", "multi_arch")

_PLATFORMS = [
    "//platforms:linux_amd64",
    "//platforms:linux_arm64",
]

def bridge_image(name, binary, base = "@distroless_base"):
    """Defines <name>_image (multi-arch) and <name>_push targets for one house binary.

    Args:
        name: bridge/service name, e.g. "plex" — must match the go_binary's own
            target name (bridges/<name>:<name> or similar), since that's also
            the binary's filename and this macro assumes both match.
        binary: label of the go_binary target to package, e.g. "//bridges/plex".
        base: base image target, "@distroless_base" (glibc, no shell — the
            default) or "@debian_slim" (adds libdbus, needed by airthings).
    """
    pkg_tar(
        name = name + "_layer",
        srcs = [binary],
        package_dir = "/usr/local/bin",
    )

    oci_image(
        name = name + "_image",
        base = base,
        tars = [":" + name + "_layer"],
        entrypoint = ["/usr/local/bin/" + name],
    )

    multi_arch(
        name = name + "_platform_images",
        image = ":" + name + "_image",
        platforms = _PLATFORMS,
    )

    oci_image_index(
        name = name + "_index",
        images = [":" + name + "_platform_images"],
    )

    oci_push(
        name = name + "_push",
        image = ":" + name + "_index",
        repository = REGISTRY + "/house/" + name,
    )
