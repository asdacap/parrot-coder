# Parrot Coder

- This is parrot-coder. Its basically a coding agent designed to be not too much.

# Environment

- Use nix flake please
- Whenever a dependency changes (`go.mod`/`go.sum` touched), the `vendorHash` in `flake.nix` goes stale and `nix run` breaks with an "inconsistent vendoring" error. Always re-run `nix run .# -- --version` after a dependency change, and if it fails, refresh `vendorHash`: set it to `sha256-AAAA...A=` (43 `A`s), run the build, and paste the `got:` hash back in.

# Coding guideline

- Pay higher attention to two of the SOLID principle, the Open-Closed principle and Dependency Inversion princible.
- Prefer interface rather than config.
- Hardcoding id match is likely an antipattern. For example, changing a behavior of the UI by checking the tool id match a specific id. This is wrong. Rather, make a method in the Tool interface that itself will change behavior. 
- On CLI Event line. Do not forget to put icon or at least indentation.
- Prefer rich domain model rather than anemic domain model.
- Postfix dto name with 'Dto'
