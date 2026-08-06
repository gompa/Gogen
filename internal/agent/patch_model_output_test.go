package agent

import (
	"path/filepath"
	"testing"
)

// Regression test for a real model-generated patch that arrived with a stray
// prose fragment before the diff and eight "*** End of file" trailers after
// the final hunk. The stray prefix is ignored, the first trailer terminates
// the patch, and the three hunks parse correctly.
func TestParseUnifiedDiffModelPatchWithRepeatedEndMarkers(t *testing.T) {
	diff := "mplete').\n" +
		"--- a/tmp/bidi_full.mjs\n" +
		"+++ b/tmp/bidi_full.mjs\n" +
		"@@ -114,9 +114,16 @@\n" +
		"     const newRes = await send('session.new', { capabilities: { alwaysMatch: { acceptInsecureCerts: true } } });\n" +
		"     console.log('session:', newRes.sessionId);\n" +
		"     const ctx = await send('browsingContext.create', { type: 'tab', url: 'http://127.0.0.1:8099/' });\n" +
		"     const context = ctx.context;\n" +
		"     console.log('tab:', context);\n" +
		"+    await send('browsingContext.navigate', {\n" +
		"+      context,\n" +
		"+      url: 'http://127.0.0.1:8099/',\n" +
		"+      wait: 'interactive',\n" +
		"+    });\n" +
		"     await sleep(3000);\n" +
		"+\n" +
		"+    // Unwrap BiDi remote values (type/value wrappers).\n" +
		"+    const unwrap = (v) => {\n" +
		"+      if (!v || typeof v !== 'object' || !v.type) return v;\n" +
		"+      switch (v.type) {\n" +
		"+        case 'string': return v.value;\n" +
		"+        case 'number': return v.value;\n" +
		"+        case 'boolean': return v.value;\n" +
		"+        case 'null': return null;\n" +
		"+        case 'undefined': return undefined;\n" +
		"+        case 'array': return (v.value || []).map(unwrap);\n" +
		"+        case 'object': {\n" +
		"+          const o = {};\n" +
		"+          for (const [k, val] of v.value || []) o[k] = unwrap(val);\n" +
		"+          return o;\n" +
		"+        }\n" +
		"+        default: return v;\n" +
		"+      }\n" +
		"+    };\n" +
		"+    const jsv = (v) => {\n" +
		"+      const u = unwrap(v);\n" +
		"+      return u && u.type ? unwrap(u) : u;\n" +
		"+    };\n" +
		" \n" +
		"     const snapshot = () => evalJS(context, `(() => {\n" +
		"@@ -129,7 +136,7 @@\n" +
		"       };\n" +
		"     })()`);\n" +
		" \n" +
		"     console.log('\\u2500\\u2500 after load \\u2500\\u2500');\n" +
		"-    console.log(JSON.stringify(await snapshot(), null, 2));\n" +
		"+    console.log(JSON.stringify(jsv(await snapshot()), null, 2));\n" +
		"@@ -142,7 +149,7 @@\n" +
		"     })()`);\n" +
		"     console.log('\\nclicked:', JSON.stringify(clickInfo));\n" +
		"     await sleep(3000);\n" +
		" \n" +
		"     console.log('\\u2500\\u2500 after click \\u2500\\u2500');\n" +
		"-    const after = await snapshot();\n" +
		"+    const after = jsv(await snapshot());\n" +
		"     console.log(JSON.stringify(after, null, 2));\n" +
		"*** End of file\n" +
		"*** End of file\n" +
		"*** End of file\n" +
		"*** End of file\n" +
		"*** End of file\n" +
		"*** End of file\n" +
		"*** End of file\n" +
		"*** End of file\n"

	files, err := parseUnifiedDiff(diff)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("files=%d want 1", len(files))
	}
	if filepath.ToSlash(normalizePatchPath(files[0].newName)) != "tmp/bidi_full.mjs" {
		t.Fatalf("path=%q", files[0].newName)
	}
	hunks := files[0].hunks
	if len(hunks) != 3 {
		t.Fatalf("hunks=%d want 3", len(hunks))
	}
	if hunks[0].oldStart != 114 || len(hunks[0].oldLines) != 8 || len(hunks[0].newLines) != 36 {
		t.Fatalf("hunk 1 = oldStart %d, old %d, new %d; want 114/8/36",
			hunks[0].oldStart, len(hunks[0].oldLines), len(hunks[0].newLines))
	}
	if hunks[1].oldStart != 129 || len(hunks[1].oldLines) != 5 {
		t.Fatalf("hunk 2 = oldStart %d, old %d; want 129/5",
			hunks[1].oldStart, len(hunks[1].oldLines))
	}
	if hunks[2].oldStart != 142 || len(hunks[2].oldLines) != 7 {
		t.Fatalf("hunk 3 = oldStart %d, old %d; want 142/7",
			hunks[2].oldStart, len(hunks[2].oldLines))
	}
}
