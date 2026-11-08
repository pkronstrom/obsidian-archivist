## Tasks

Task tracker: aven

Open work lives in Aven (project `obsidian-archivist`). `aven list --ready` isn't sorted by
priority, so check `aven list --ready --priority urgent`, then `high`, then the rest.
File follow-ups you discover as `--status inbox`; the user triages them.
Keep Aven refs out of commits, PRs and committed docs; tasks point at repo paths and SHAs.

To add a task, run this from the repo, which sets the project (there are no assignees):
`aven add "<verb-first title>" --status <status> --priority <priority> --label <label> --description "<problem, done-when, repo paths>"`

- **Status:** `todo` when the user asked for it and it can start, `backlog` for later, `active` once started.
- **Priority:** `urgent` for a live failure, data loss or security exposure; `high` if overdue, due within two weeks, or blocking `high` work; `medium` for planned work with a clear next step; `low` for an improvement with no driver; `none` for an unranked idea.
- **Labels:** `server`, `plugin`, `relay`, `ci`. Other labels fail until created with `aven label create`, so add one only for a lasting area.
- **Epic:** if it serves one in `aven list --epics`, run `aven epic add <task> <epic>` afterwards. Create an epic only for three or more tasks toward one result that will finish.
- **Order and dates:** `aven dep add <waiting> <blocker>` when one must finish first; `--available-at <date>` for not before; `--due <date>` for a true deadline.
