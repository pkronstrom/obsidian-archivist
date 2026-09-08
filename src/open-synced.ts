/** Resolve a deep-linked note only after a completed sync; never create it. */
export async function openSyncedNote(
    path: string,
    sync: () => Promise<void>,
    exists: (path: string) => boolean,
    open: (path: string) => Promise<void>,
): Promise<void> {
    if (!path || path.startsWith('/') || path.includes('\\') || path.split('/').some(p => p === '..' || p === '.') || !path.endsWith('.md')) {
        throw new Error('Invalid note path');
    }
    await sync();
    if (!exists(path)) throw new Error('Note is not available after syncing. Check Archivist connection and try again.');
    await open(path);
}
