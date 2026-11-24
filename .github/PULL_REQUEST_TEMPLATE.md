## Summary

<!-- Describe what this PR changes and why. -->

## Related Issue

<!-- Closes #123 -->

## Type of Change

- [ ] Bug fix / protocol discrepancy fix
- [ ] New Feishu Open API endpoint or event
- [ ] Control plane / CLI / Web UI enhancement
- [ ] Test coverage / fixture / documentation update
- [ ] Refactor / chore

## Protocol Fidelity Checklist

- [ ] Request/response schema, headers, and error codes match real Feishu Open Platform behavior
- [ ] Strict mode validation added for invalid inputs / auth errors
- [ ] Unit tests and/or official SDK integration tests added (`go test -race ./...`)
- [ ] Verified against a real Feishu test tenant (if applicable) and sanitized golden fixtures added under `testdata/fixtures/`
- [ ] Documentation / compatibility matrix updated (if applicable)
- [ ] No real credentials, secrets, or unsanitized tenant payloads committed
