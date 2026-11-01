# Admin console locales

`en.json` is the complete fallback; `ko.json`, `ja.json`, `zh-Hans.json` and `es.json` are complete
bundles too. `en` and `ko` are generated from `internal/transport/web/ui/scripts/strings.py`; do not
edit those generated files directly. The other bundles are maintained as plain JSON files.

To add another console language:

1. Add `locales/ui/<BCP-47 tag>.json` with translated keys from `en.json`. A partial bundle is valid;
   missing strings use English. Prefer the base tag used by the bot pack (`fr`, not `fr-CA`) unless
   the console intentionally needs a regional variant.
2. Translate visible prose only. Keep key names, `{name}` placeholders (including repeated occurrences),
   literal brace examples such as `{bot}`, and any JSON examples intact. Translate a count label and
   all its `.one`/`.other` variants together to avoid mixing English and the new language.
3. Run `npm run test:i18n`, `npm run check -- --fail-on-warnings`, and `npm run build` in
   `internal/transport/web/ui`. The build validates catalogs and refreshes `ui/dist`; include the
   generated files when shipping a new language.
4. Review the language selector, long labels, counts, dates and errors at desktop and 320px widths.
   RTL language tags set document direction automatically; test both layouts and keyboard navigation.

The server discovers these files dynamically and merges them over English. Language names use the
browser's `Intl.DisplayNames` when a bundle does not define `lang.<tag>`. The structural checks cannot
verify translation meaning or every constructed runtime key; review a running console in the target
language before release. See [the product language guide](../../docs/tools-and-languages.md#6-언어)
for bot prompts, operational messages, search rules and machine-translation precautions.
