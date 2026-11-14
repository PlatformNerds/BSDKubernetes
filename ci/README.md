# CI templates

`freebsd-cross-build.yml` is the nightly FreeBSD cross-compile workflow. It's kept here because the token
used to bootstrap the repo lacked the `workflow` scope. To enable it:

```sh
gh auth refresh -h github.com -s workflow
mkdir -p .github/workflows && git mv ci/freebsd-cross-build.yml .github/workflows/ && git commit -m "Enable CI" && git push
```
