// Single entry point for the integration harness.
//
// sync.ts and client.ts must be bundled TOGETHER. Bundled separately, each gets
// its own copy of client.ts, so `err instanceof UnknownBaseError` compares
// against two different class objects and the 409 recovery path never runs.
// The plugin itself is one bundle, so this only ever bit the harness -- but it
// bit it silently, as an unhandled rejection three tests later.
export { Sync, skip, conflictName } from "../src/sync";
export { Client, UnknownBaseError } from "../src/client";
