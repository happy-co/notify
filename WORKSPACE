workspace(name = "com_github_rjeczalik_notify")

git_repository(
    name = "io_bazel_rules_go",
    commit = "d5881337910b23d4f7ca83a8ae2ca52001f438fe",
    remote = "https://github.com/happy-co/rules_go.git",
)

load("@io_bazel_rules_go//go:def.bzl", "go_repositories", "new_go_repository")

go_repositories()

new_go_repository(
    name = "org_golang_x_sys",
    commit = "4cd6d1a821c7175768725b55ca82f14683a29ea4",
    importpath = "golang.org/x/sys",
)
