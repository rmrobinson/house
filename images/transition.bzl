"""A rule transitioning an oci_image to multiple platforms.

Copied from rules_oci's own multi_architecture_image example
(examples/multi_architecture_image/transition.bzl) — rules_oci doesn't ship
this as part of the rule set itself, it's the documented pattern for
consumers to build a multi-arch oci_image_index.
"""

def _multiarch_transition(settings, attr):
    return [
        {"//command_line_option:platforms": str(platform)}
        for platform in attr.platforms
    ]

multiarch_transition = transition(
    implementation = _multiarch_transition,
    inputs = [],
    outputs = ["//command_line_option:platforms"],
)

def _impl(ctx):
    return DefaultInfo(files = depset(ctx.files.image))

multi_arch = rule(
    implementation = _impl,
    attrs = {
        "image": attr.label(cfg = multiarch_transition),
        "platforms": attr.label_list(),
        "_allowlist_function_transition": attr.label(
            default = "@bazel_tools//tools/allowlists/function_transition_allowlist",
        ),
    },
)
