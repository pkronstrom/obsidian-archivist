# Diagrams

**The README inlines the mermaid source directly**, so GitHub renders it: the
diagram zooms, diffs as text, and needs no regeneration step. `architecture.mmd`
is kept as the canonical copy and `architecture.svg` as a fallback, because the
tradeoff below is real and may send us back to the image.

Edit the source, then regenerate the SVG and paste the source into the README's
"How it flows" block:

```bash
npx -y @mermaid-js/mermaid-cli \
  -i docs/diagrams/architecture.mmd \
  -o docs/diagrams/architecture.svg \
  -c docs/diagrams/mermaid-config.json \
  -b white
```

## The tradeoff, and why it went back and forth

The README carried the source, then the SVG, and now the source again. What
each costs:

**The layout wants ELK.** Mermaid's default engine is dagre, and dagre staggers
the two subgraphs diagonally and routes the device edges in long swoops across
the whole figure. ELK lays the same source out compactly with orthogonal edges.
GitHub's mermaid build does not ship the ELK layout, so the inlined copy falls
back to dagre. That is the price of inlining, and it is worth re-checking on the
rendered page rather than assuming: if dagre's layout is unreadable, put the
SVG back.

**Labels must not be `foreignObject`** *(only matters for the SVG path).*
Mermaid wraps every label in
`<foreignObject>` by default. That renders fine in a browser and as blank space
when GitHub serves an SVG through its image proxy — the diagram would arrive with
no text at all. `htmlLabels: false` in `mermaid-config.json` emits native `<text>`
elements instead.

Both settings live in `mermaid-config.json`. Passing them as an `%%{init: ...}%%`
directive inside the `.mmd` also works, but comments above such a directive make
the parser fail, so the config file is the safer home.

## Why it is portrait

`flowchart TB`, not `LR`. The left-to-right version came out 2344 x 465, which
GitHub scales down to its ~800px column and renders at roughly a third size —
legible only if you open the image on its own. Portrait is 602 x 1129, so it
lands at full size and stays readable inline.

The relay has no subgraph box of its own, deliberately. When it had one, the two
device edges ran vertically straight through it on their way to the server,
which read as though Obsidian synced via the relay. The "usually here, can run
anywhere" note moved into the relay node's own label instead.

## Gotchas when editing

- **No HTML in labels.** `htmlLabels: false` means `<i>` and `<b>` print
  literally. `<br/>` still works for line breaks.
- **Check the render, don't trust the source.** Add `-o /tmp/check.png -s 2` to
  the command above and look at it. Layout changes are not predictable from a
  diff.
