# RBAC Configs

This repo hosts RBAC configs for all things related to Space Chunks.

## Structure

We group all available roles for each project in a dedicated file. The file should be named after the project.

## Naming guidelines

To enforce consistent role and permission naming across projects/plugins, we define the following format:

```
spacechunks.<plugin>.<components>.<permission>
```

All permission should be Kebab Case prefixed with `spacechunks.`. Roles should simply be in Kebap Case. Prefer adjectives 
over nouns, when choosing names for permissions i.e. `spacechunks.plugin.enabled` instead of `spacechunks.plugin.enable`. 
Only use letters found in the English alphabet.

For example:

**GOOD**
```
spacechunks.lobby.chunk-viewer.enabled
spacechunks.party.kick-member
```

**BAD**
```
Lobby.use_item
this_is-myspecial.pörmission
```