import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import * as api from "../../dist-test/entry.mjs";
import { FakeApp } from "../obsidian-shim.mjs";
const a = "11111111-1111-1111-1111-111111111111";
const b = "22222222-2222-2222-2222-222222222222";
const c = "33333333-3333-3333-3333-333333333333";
const plugin = (id, version="1", desktop=false) => ({id, name:id, version, isDesktopOnly:desktop});
const record = (id, plugins=[], platform="desktop") => ({schemaVersion:1, installationId:id, deviceName:"Mac", platform, updatedAt:"2026-09-09T12:00:00.000Z", plugins});
const file = id => `.archivist/plugin-inventory/${id}.json`;
async function appFor(t) {
 const root = await fs.mkdtemp(path.join(os.tmpdir(), "inventory-"));
 t.after(() => fs.rm(root,{recursive:true,force:true}));
 const app = new FakeApp(root); app.vault.configDir = ".custom"; return app;
}
test("inventory path is the only hidden exception", () => {
 assert.equal(typeof api.inventoryPathAllowed, "function");
 assert.equal(api.inventoryPathAllowed(file(a)), true);
 assert.equal(api.skip(file(a)), false);
 for (const p of [".archivist/private.json", file(a)+"/x", file(a).replace(a,"x"),file(a).replace(".json",".conflict-mac.json"),"x/"+file(a),".archivist/../"+file(a), file(a).replace(".json",".local.json")]) {
  assert.equal(api.inventoryPathAllowed(p),false,p); assert.equal(api.skip(p),true,p);
 }
});
test("parse validates identity/schema/bounds/duplicates and projects fields", () => {
 assert.equal(typeof api.parseInventory,"function");
 const r = record(a,[{...plugin("foo"), secret:"never"}]); r.extra="never";
 assert.deepEqual(api.parseInventory(file(a),JSON.stringify(r)),record(a,[plugin("foo")]));
 for (const value of [{...r,schemaVersion:2},{...r,installationId:b},{...r,plugins:[plugin("foo"),plugin("foo")]},{...r,updatedAt:"not a date"},{...r,deviceName:"x".repeat(201)},{...r,plugins:[plugin("../foo")]}]) assert.equal(api.parseInventory(file(a),JSON.stringify(value)),null);
 assert.equal(api.parseInventory(file(a),"{"),null);
 assert.equal(api.parseInventory(file(a)," ".repeat(1_048_577)),null);
});
test("publication includes disabled manifests, projects fields and preserves idle bytes", async t => {
 assert.equal(typeof api.publishInventory,"function");
 const app = await appFor(t), other = await appFor(t);
 const id = api.installationId(app);
 assert.equal(api.installationId(app),id);
 assert.notEqual(api.installationId(other),id);
 await app.vault.adapter.write(".custom/plugins/z/manifest.json",JSON.stringify({...plugin("z"), settings:{password:"secret"}}));
 await app.vault.adapter.write(".custom/plugins/z/data.json",'{"password":"secret"}');
 await app.vault.adapter.write(".custom/plugins/a/manifest.json",JSON.stringify(plugin("a")));
 const first = await api.publishInventory(app,"Mac","desktop");
 assert.deepEqual(first.plugins,[plugin("a"),plugin("z")]);
 const before = await app.vault.adapter.read(file(id));
 await api.publishInventory(app,"Mac","desktop");
 assert.equal(await app.vault.adapter.read(file(id)),before);
 assert.ok(!before.includes("secret"));
 await app.vault.adapter.write(file(b),JSON.stringify(record(b)));
 await app.vault.adapter.write(".custom/plugins/z/manifest.json",JSON.stringify(plugin("z","2")));
 assert.equal((await api.publishInventory(app,"Renamed","desktop")).installationId,id);
 assert.equal(await app.vault.adapter.read(file(b)),JSON.stringify(record(b)));
 await app.vault.adapter.rmdir(".custom/plugins/a",true);
 assert.deepEqual((await api.publishInventory(app,"Renamed","desktop")).plugins,[plugin("z","2")]);
});
test("incomplete scan leaves last publication intact", async t => {
 assert.equal(typeof api.publishInventory,"function");
 const app = await appFor(t);
 await app.vault.adapter.write(".custom/plugins/a/manifest.json",JSON.stringify(plugin("a")));
 const first = await api.publishInventory(app,"Mac","desktop");
 const before=await app.vault.adapter.read(file(first.installationId));
 await app.vault.adapter.write(".custom/plugins/a/manifest.json","{");
 await assert.rejects(api.publishInventory(app,"Mac","desktop"),/inventory/i);
 assert.equal(await app.vault.adapter.read(file(first.installationId)),before);
});
test("comparison supports duplicate labels, many devices, versions and compatibility", () => {
 assert.equal(typeof api.compareInventories,"function");
 const local=record(a,[plugin("same"),plugin("diff","beta"),plugin("local")],"mobile");
 const peers=[record(b,[plugin("same"),plugin("diff","dev"),plugin("missing"),plugin("desktop","1",true),plugin("uncertain","1",true)]),record(c,[plugin("diff","other"),plugin("missing"),plugin("uncertain","1",false)])];
 const rows=api.compareInventories(local,peers);
 const get=id=>rows.find(r=>r.id===id);
 assert.equal(get("same").status,"installed");
 assert.equal(get("diff").status,"different-version");
 assert.equal(get("diff").versions.length,2);
 assert.equal(get("missing").status,"missing");
 assert.equal(get("missing").versions[0].devices.length,2);
 assert.equal(get("desktop").status,"desktop-only");
 assert.equal(get("uncertain").compatibility,"uncertain");
 assert.equal(get("local").versions.length,0);
 assert.equal(api.compareInventories(local,peers,c).find(r=>r.id==="desktop"),undefined);
 assert.equal(api.compareInventories(local,[]).length,3);
});

test("old-server inventory refusal is reported while notes can sync", async t => {
 const app=await appFor(t);
 await app.vault.adapter.write("note.md","note");
 await app.vault.adapter.write(file(a),JSON.stringify(record(a)));
 const pushed=[];
 const client={
  async index(){return {service:"archivist",protocol:2,version:"t",vault:"personal"};},
  async head(){return "";}, async changes(){return {head:"",entries:[]};},
  async missing(h){return h;},async putContent(){},
  async push(base,device,changes){pushed.push(...changes);return {head:"head",results:changes.map(c=>({path:c.path,status:c.path.startsWith(".archivist")?"refused":"applied"}))};},
 };
 const sync=new api.Sync(app,()=>client,()=>"Mac");
 const report=await sync.run();
 assert.equal(report.refused?.[0]?.path,file(a));
 assert.equal(report.pushed,1);
 assert.ok(pushed.some(c=>c.path==="note.md"));
 assert.equal(app.loadLocalStorage("archivist.state").files[file(a)],undefined);
});

test("oversized UTF-8 inventory preserves prior publication instead of causing churn", async t => {
 const app=await appFor(t);
 const first=await api.publishInventory(app,"Mac","desktop");
 const before=await app.vault.adapter.read(file(first.installationId));
 const adapter=app.vault.adapter;
 const oldExists=adapter.exists.bind(adapter),oldRead=adapter.read.bind(adapter),oldStat=adapter.stat.bind(adapter),oldList=adapter.list.bind(adapter);
 adapter.exists=async p=>p===".custom/plugins" || p.endsWith("manifest.json") || oldExists(p);
 adapter.list=async p=>p===".custom/plugins" ? {files:[],folders:Array.from({length:1600},(_,n)=>`.custom/plugins/p${n}`)} : oldList(p);
 adapter.stat=async p=>p.endsWith("manifest.json")?{type:"file",size:1000}:oldStat(p);
 adapter.read=async p=>p.endsWith("manifest.json")?JSON.stringify({id:p.split("/")[2],name:"界".repeat(200),version:"界".repeat(100),isDesktopOnly:false}):oldRead(p);
 await assert.rejects(api.publishInventory(app,"Mac","desktop"),/inventory/i);
 assert.equal(await oldRead(file(first.installationId)),before);
});

test("failed replacement restores the previous inventory and startup recovers an interrupted replacement",async t=>{
 const app=await appFor(t);
 await app.vault.adapter.write(".custom/plugins/a/manifest.json",JSON.stringify(plugin("a")));
 const first=await api.publishInventory(app,"Mac","desktop");
 const target=file(first.installationId);
 const backup=`.archivist/plugin-inventory/.archivist-tmp-${first.installationId}.previous`;
 const before=await app.vault.adapter.read(target);
 await app.vault.adapter.write(".custom/plugins/a/manifest.json",JSON.stringify(plugin("a","2")));
 const rename=app.vault.adapter.rename.bind(app.vault.adapter);
 app.vault.adapter.rename=async (from,to)=>{
  if (from.endsWith(first.installationId) && to===target) throw new Error("interrupted replacement");
  return rename(from,to);
 };
 await assert.rejects(api.publishInventory(app,"Mac","desktop"));
 assert.equal(await app.vault.adapter.read(target),before);
 app.vault.adapter.rename=rename;
 await rename(target,backup);
 await app.vault.adapter.write(".custom/plugins/a/manifest.json","{");
 await assert.rejects(api.publishInventory(app,"Mac","desktop"),/incomplete/);
 assert.equal(await app.vault.adapter.read(target),before);
});

test("settings-only plugin folders left by config sync do not break inventory", async t => {
 const app=await appFor(t);
 await app.vault.adapter.write(".custom/plugins/installed/manifest.json",JSON.stringify(plugin("installed")));
 await app.vault.adapter.write(".custom/plugins/not-installed/data.json",'{"setting":"local"}');
 const inventory=await api.publishInventory(app,"Phone","mobile");
 assert.deepEqual(inventory.plugins,[plugin("installed")]);
 assert.equal(await app.vault.adapter.read(".custom/plugins/not-installed/data.json"),'{"setting":"local"}');
});
test("scan errors identify the offending manifest and retain the old inventory",async t=>{
 const app=await appFor(t);
 await app.vault.adapter.write(".custom/plugins/broken/manifest.json",JSON.stringify(plugin("broken")));
 const first=await api.publishInventory(app,"Phone","mobile");
 const before=await app.vault.adapter.read(file(first.installationId));
 await app.vault.adapter.write(".custom/plugins/broken/manifest.json","{");
 await assert.rejects(api.publishInventory(app,"Phone","mobile"),/broken.*manifest\.json/);
 assert.equal(await app.vault.adapter.read(file(first.installationId)),before);
});
