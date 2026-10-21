# Regenerates src/tokens.css from ukubi/tokens.json, the Ukubi design system's
# tokens (https://claude.ai/artifact/B8nzjukaMY8HRQnjLRRKMc). Run from islands/:
#   python3 ukubi/gen.py
import json

t = json.load(open("ukubi/tokens.json"))
out = [
    "/* Generated from ukubi/tokens.json, the Ukubi design system",
    "   (https://claude.ai/artifact/B8nzjukaMY8HRQnjLRRKMc). Do not edit by hand:",
    "   refresh tokens.json from the artifact and rerun ukubi/gen.py. */",
    ":host {",
    f"  --ukubi-font-sans: {t['type']['families']['sans']};",
    f"  --ukubi-font-mono: {t['type']['families']['mono']};",
]
for g in t["type"]["groups"]:
    for st in g["styles"]:
        if st["name"].startswith("arabic"):
            continue
        for key, prop in (("size", "fontSize"), ("line", "lineHeight"), ("weight", "fontWeight")):
            out.append(f"  --ukubi-{st['name']}-{key}: {st[prop]};")
for k in ("spacing", "radius", "chamfer"):
    for tok in t[k]["tokens"]:
        out.append(f"  --ukubi-{tok['name']}: {tok['value']};")
out.append("}")
# office is the default; the theme attribute switches.
for sel, theme in ((':host, :host([theme="office"])', "office"), (':host([theme="underground"]), :host(:not([theme])[data-fleet-scheme="dark"])', "underground"), (':host([theme="glow"])', "glow")):
    out.append(sel + " {")
    out.append(f"  color-scheme: {'light' if theme == 'office' else 'dark'};")
    out += [f"  --ukubi-{c['name']}: {c['value'][theme]};" for c in t["color"]["tokens"]]
    out += [f"  --ukubi-{s['name']}: {s['value'][theme]};" for s in t["shadow"]["tokens"]]
    out.append("}")
open("src/tokens.css", "w").write("\n".join(out) + "\n")
