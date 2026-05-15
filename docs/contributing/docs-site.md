---
title: Docs Site
weight: 4
---

The documentation site is built with [Hugo](https://gohugo.io/) using the [Hextra](https://imfing.github.io/hextra/) theme. The Hugo project lives in `website/` and reads content from `docs/` via module mounts.

## Local development

Start the dev server (Mage downloads Hugo automatically):

```bash
mage docsServe
```

This starts the Hugo dev server at [http://localhost:1313](http://localhost:1313) with live reload.

## Building the site

Build the static site to `website/public/`:

```bash
mage docs
```

## Content structure

Documentation pages live in the `docs/` directory at the repository root. Hugo reads them directly through module mounts configured in `website/`, so there is no need to copy files into `website/content/`.

Each section has an `_index.md` that defines the section title and sort order, with individual pages alongside it:

```
docs/
  getting-started/
    _index.md
    installation.md
    quick-start.md
    configuration.md
  guides/
    _index.md
    ...
  contributing/
    _index.md
    dev-setup.md
    project-layout.md
    ...
```

## Adding a new page

Create a markdown file in the appropriate section directory with YAML front matter:

```yaml
---
title: "Your Page Title"
weight: 3
---

Page content goes here.
```

- **`title`** -- displayed in the sidebar and page header.
- **`weight`** -- controls sort order within the section (lower numbers appear first).

For a new section, create a directory with an `_index.md`:

```yaml
---
title: "Section Name"
weight: 4
---

Short description of this section.
```
