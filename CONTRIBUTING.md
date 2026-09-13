# Contributing

Thank you for your interest in contributing to this project!

## Getting Started

1. Fork the repository and create a feature branch
2. Make your changes following existing code patterns
3. Test your changes
4. Submit a pull request with a clear description

## What to Contribute

- Bug fixes and improvements
- Documentation updates
- Test coverage improvements
- Performance optimizations

## Guidelines

- Follow the existing code style
- Include tests for new functionality when applicable
- Update documentation if needed
- Keep changes focused and atomic

## Questions?

- Check existing issues first
- For bugs: Create an issue with reproduction steps
- For questions: Email support@revenium.io

## Security

For security vulnerabilities, please follow our [Security Policy](https://github.com/revenium/revenium-bifrost-plugin/blob/HEAD/SECURITY.md) - do not create public issues.

## License

By contributing, you agree your contributions will be licensed under the same license as this project.

## Release Procedure (Maintainers)

Public releases are cut from the internal repo via the export script. See `export_public_repo.sh` for the implementation.

Steps:

1. `git clone git@github.com:revenium/revenium-bifrost-plugin-internal.git`
2. `cd revenium-bifrost-plugin-internal && ./export_public_repo.sh`
3. `cd $WORKDIR/revenium-bifrost-plugin` (path printed by the script)
4. Review `git status` and diff against the previous public revision — this is the audit gate
5. `git init && git remote add origin git@github.com:revenium/revenium-bifrost-plugin.git`
6. `git add -A && git commit -m "release v0.1.0"`
7. `git push -u origin master`
8. `git tag v0.1.0 && git push --tags` — fires `release.yml` to build + publish the 4 signed artifacts
