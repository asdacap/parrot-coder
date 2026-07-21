# Parrot Coder

- This is parrot-coder. Its basically a coding agent designed to be not too much.

# Environment

- Use nix flake please

# Coding guideline

- Pay higher attention to two of the SOLID principle, the Open-Closed principle and Dependency Inversion princible.
- Prefer interface rather than config.
- Hardcoding id match is likely an antipattern. For example, changing a behavior of the UI by checking the tool id match a specific id. This is wrong. Rather, make a method in the Tool interface that itself will change behavior. 
