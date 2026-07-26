# Parrot Coder

- This is parrot-coder. Its basically a coding agent designed to be not too much.

# Environment

- Use nix flake please
- Whenever a dependency changes (`go.mod`/`go.sum` touched), the `vendorHash` in `flake.nix` goes stale and `nix run` breaks with an "inconsistent vendoring" error. Always re-run `nix run .# -- --version` after a dependency change, and if it fails, refresh `vendorHash`: set it to `sha256-AAAA...A=` (43 `A`s), run the build, and paste the `got:` hash back in.
- TestPersistentProcessPTYInputPollingOwnershipAndCleanup will fail in sandbox. Just leave it.

# Coding guideline

- Pay higher attention to two of the SOLID principle, the Open-Closed principle and Dependency Inversion princible.
- Prefer interface rather than config.
- Hardcoding id match is likely an antipattern. For example, changing a behavior of the UI by checking the tool id match a specific id. This is wrong. Rather, make a method in the Tool interface that itself will change behavior. 
- On CLI Event line. Do not forget to put icon or at least indentation.
- Prefer rich domain model rather than anemic domain model.
- Postfix dto name with 'Dto'
- Rather than adding method to a repository or a container that operate on the underlying object, add that method to the object itself. For example, rather than repo.SendToAgent(), do repo.GetAgent().Send().
- If you add a small structure to get to a large structure to make things smaller, you just did a stupid thing. The net impact is an increase in code. Same thing with adding a simple function, while a more complicated function achieve the same thing and you cannot remove the complicated function. That make things worst, you just added more code! Just call the complicated function and remove the simple function! And don't you complain about performance to me, I'm gonna slap your virtual ass!
- ObjectA.ChildSomething() is VERY stupid when the child in question is ObjectA itself! Just ObjectA.Something()!
- Do not directly update database outside the relevant runtime domain model. EG: Do not add message history database manually outside of AgentSession.
- Do not put dependency in a config struct. Pass it in the struct directly in the constructor so method does not go through config to get dependency.
- Calling a method is better than hooking up a mutable state and a callback and then doing the whole orchestration of making the mutable state in sync. Just call a method from the original source when you need it.
- Prefer noop than nil.
