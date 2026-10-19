# Contributing

Issues and pull requests are welcome.

```
make test        # go vet and go test -race
make demo        # fictional mailbox, no network
make install-app # macOS desktop app into ~/Applications
```

- Keep changes narrow and add a test with every fix. Mail is unforgiving: threading, flag sync and sending have regression tests; extend them.
- Errors must name their cause; no silent failures.
- UI text is short and has no filler. Keep the interface keyboard- and screen-reader-operable.
- Never commit real mail, addresses or credentials; fixtures use `example.com`.
