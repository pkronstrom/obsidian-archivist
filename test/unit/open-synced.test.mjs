import { test } from 'node:test';
import assert from 'node:assert/strict';
import { openSyncedNote } from '../../dist-test/entry.mjs';
test('deep link waits for sync before resolving and opening', async () => {
    let available=false; const calls=[];
    await openSyncedNote('Inbox/Plaud/idea.md', async()=>{calls.push('sync');available=true;},()=>available,async()=>{calls.push('open');});
    assert.deepEqual(calls,['sync','open']);
});
test('missing or unsafe notes are never opened or created', async () => {
    let opens=0;
    await assert.rejects(openSyncedNote('Inbox/missing.md',async()=>{},()=>false,async()=>{opens++;}), /not available/);
    await assert.rejects(openSyncedNote('../outside.md',async()=>{},()=>true,async()=>{opens++;}), /Invalid/);
    assert.equal(opens,0);
});
test('sync failure cannot open a stale or missing note',async()=>{
    let opened=false;
    await assert.rejects(openSyncedNote('note.md',async()=>{throw Error('offline');},()=>true,async()=>{opened=true;}),/offline/);
    assert.equal(opened,false);
});
