import {test} from "node:test";
import assert from "node:assert/strict";
import * as api from "../../dist-test/entry.mjs";
class Element {
 constructor(tag="div", opts={}) {this.tag=tag;this.opts=opts;this.children=[];this.listeners={};this.value="";}
 createEl(tag,opts={}){const el=new Element(tag,opts); this.children.push(el);return el;}
 createDiv(opts={}){return this.createEl("div",opts);}
 empty(){this.children=[];}
 addEventListener(name,fn){this.listeners[name]=fn;}
 setAttribute(name,value){this.opts.attr??={};this.opts.attr[name]=value;}
 addClass(cls){this.opts.cls=((this.opts.cls??"")+" "+cls).trim();}
 get text(){return [this.textContent??this.opts.text??"",...this.children.map(c=>c.text)].join(" ");}
 all(tag){return [...(this.tag===tag?[this]:[]),...this.children.flatMap(c=>c.all(tag))];}
}
const a="11111111-1111-1111-1111-111111111111",b="22222222-2222-2222-2222-222222222222",c="33333333-3333-3333-3333-333333333333";
const p=(id,version="1",desktop=false)=>({id,name:id,version,isDesktopOnly:desktop});
const r=(id,plugins=[],platform="desktop")=>({schemaVersion:1,installationId:id,deviceName:"Mac",updatedAt:"2026-09-09T12:00:00.000Z",platform,plugins});
test("inventory view groups device versions, filters and opens only official store URLs",()=>{
 assert.equal(typeof api.renderInventoryView,"function");
 const el=new Element(); let filter,refreshed=0; const opened=[];
 api.renderInventoryView(el,{local:r(a,[p("diff","local")],"mobile"),inventories:[r(b,[p("diff","remote"),p("missing"),p("desktop","1",true)]),r(c,[p("missing")])],unavailable:1},"",v=>filter=v,()=>refreshed++,url=>opened.push(url));
 assert.match(el.text,/Missing on this device/);
 assert.match(el.text,/Different version/);
 assert.match(el.text,/Desktop only/);
 assert.match(el.text,/22222222/); assert.match(el.text,/33333333/);
 assert.match(el.text,/Inventory updated/);
 assert.match(el.text,/unavailable/);
 const select=el.all("select")[0]; select.value=c;select.listeners.change();assert.equal(filter,c);
 el.all("button")[0].listeners.click();assert.equal(refreshed,1);
 const storeButtons=el.all("button").filter(b=>b.opts.attr?.["data-icon"]==="external-link");
 assert.equal(storeButtons.length,2);
 for (const button of storeButtons) button.listeners.click();
 assert.ok(opened.every(url=>url.startsWith("https://community.obsidian.md/plugins/")));
 assert.equal(api.communityPluginUrl("x/?&"),"https://community.obsidian.md/plugins/x%2F%3F%26");
});
test("empty and unavailable inventories never claim all devices match",()=>{
 assert.equal(typeof api.renderInventoryView,"function");
 const el=new Element();
 api.renderInventoryView(el,{local:r(a),inventories:[],unavailable:0},"",()=>{},()=>{});
 assert.match(el.text,/No other device has shared/);
 assert.doesNotMatch(el.text,/all.*match/i);
});
test("remote strings are text and mobile uncertainty is visible",()=>{
 assert.equal(typeof api.renderInventoryView,"function");
 const el=new Element(); const x=p("x");x.name="<img onerror=bad>";
 api.renderInventoryView(el,{local:r(a,[],"mobile"),inventories:[r(b,[{...x,isDesktopOnly:true}]),r(c,[x])],unavailable:0},"",()=>{},()=>{});
 assert.equal(el.all("img").length,0);assert.match(el.text,/<img onerror=bad>/);assert.match(el.text,/Compatibility uncertain/);
});
