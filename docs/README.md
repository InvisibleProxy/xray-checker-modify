# Documentation site

Каталог содержит унаследованный от upstream многоязычный сайт на Astro + Starlight. Эксплуатационные особенности текущего форка описаны в корневых [`README.md`](../README.md) и [`PROJECT_CONTEXT.md`](../PROJECT_CONTEXT.md); этот сайт не должен незаметно переопределять их бизнес-правила.

## Разработка

Требования: Node.js и pnpm 9.

```powershell
Set-Location docs
pnpm install --frozen-lockfile
pnpm dev
```

Production build:

```powershell
Set-Location docs
pnpm install --frozen-lockfile
pnpm build
```

Исходники страниц находятся в `src/content/docs/`. Английские страницы лежат в корне разделов, переводы — в `ru/` и `fa/`.
