import { test } from "node:test";
import assert from "node:assert/strict";
import { configSyncable, DEFAULT_CONFIG_SYNC, skip } from "../../dist-test/entry.mjs";

test("legacy config choices cannot reactivate config transfer", () => {
 for (const level of ["files", "appearance", "plugins"]) {
  const config = {level, acceptedPlugins: ["dataview"], acceptAllPlugins: true};
  for (const p of [".obsidian/app.json", ".obsidian/appearance.json", ".obsidian/hotkeys.json", ".obsidian/community-plugins.json", ".obsidian/core-plugins.json", ".obsidian/plugins/dataview/data.json", ".obsidian/themes/Minimal/theme.css", ".obsidian/snippets/dark.css"]) {
   assert.equal(configSyncable(p, config), false, p);
   assert.equal(skip(p, config), true, p);
  }
  assert.equal(skip("notes/a.md", config), false);
 }
});
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { Sync } from "../../dist-test/entry.mjs";
import { FakeApp } from "../obsidian-shim.mjs";

test("turning the level DOWN does not delete the config off the server", async () => {
	const root = await fs.mkdtemp(path.join(os.tmpdir(), "downgrade-"));
	const app = new FakeApp(root);

	// This device synced config at the appearance level, so its snapshot holds
	// the config paths.
	app.saveLocalStorage("archivist.state", {
		base: "serverhead",
		vault: "personal",
		files: {
			"notes/a.md": { hash: "h1", mtime: 1, size: 1 },
			".obsidian/appearance.json": { hash: "h2", mtime: 1, size: 1 },
			".obsidian/snippets/dark.css": { hash: "h3", mtime: 1, size: 1 },
		},
	});
	await fs.mkdir(path.join(root, "notes"), { recursive: true });
	await fs.writeFile(path.join(root, "notes/a.md"), "still here\n");

	// Now it is set back to Files only.
	const level = { ...DEFAULT_CONFIG_SYNC, level: "plugins", acceptAllPlugins: true };
	const pushes = [];
	const client = {
		async index() {
			return { service: "archivist", version: "t", protocol: 2, vault: "personal" };
		},
		async head() {
			return "serverhead";
		},
		async changes() {
			return { head: "serverhead", entries: [] };
		},
		async missing(h) {
			return h;
		},
		async putContent() {},
		async push(base, device, changes) {
			pushes.push(...changes);
			return { head: "serverhead", results: [] };
		},
	};
	const sync = new Sync(app, () => client, () => "mac", () => {}, () => level);
	await sync.run();

	const deleted = pushes.filter((c) => c.op === "del").map((c) => c.path);
	assert.deepEqual(
		deleted.filter((p) => p.startsWith(".obsidian/")),
		[],
		"downgrading the level pushed deletes for config, which would wipe it for every device",
	);
});

test("pull and bootstrap preserve live config despite legacy choices", async t => {
 const root = await fs.mkdtemp(path.join(os.tmpdir(),"config-migration-"));
 t.after(()=>fs.rm(root,{recursive:true,force:true}));
 const app=new FakeApp(root);
 const configPath=".obsidian/community-plugins.json";
 await app.vault.adapter.write(configPath,'["local"]');
 const client={
  async index(){return {service:"archivist",protocol:2,version:"t",vault:"personal"};},
  async head(){return "head";},
  async changes(){return {head:"head",entries:[{op:"put",path:configPath,hash:"remote"},{op:"del",path:".obsidian/app.json"}]};},
  async snapshot(){return {head:"head",files:{[configPath]:{hash:"remote",size:10}}};},
  async getContent(){throw new Error("must not download live config");},
 };
 const sync=new Sync(app,()=>client,()=>"Mac",()=>{},()=>({level:"plugins",acceptedPlugins:[],acceptAllPlugins:true}));
 await sync.run();
 assert.equal(await app.vault.adapter.read(configPath),'["local"]');
 const {UnknownBaseError}=await import("../../dist-test/entry.mjs");
 client.changes=async()=>{throw new UnknownBaseError();};
 await sync.forceRebootstrap();
 assert.equal(await app.vault.adapter.read(configPath),'["local"]');
 assert.deepEqual(Object.keys(app.loadLocalStorage("archivist.state").files),[]);
});
