package remote

// The templates of index.html, copied from upstream admin/index.lua.
const (
	// indexHeader opens the page (admin/index.lua index_header).
	indexHeader = `<!DOCTYPE html PUBLIC "-//W3C//DTD HTML 4.01 Transitional//EN">
<html>
<head>
<title>Available rocks</title>
<meta http-equiv="content-type" content="text/html; charset=iso-8859-1">
<style>
body {
   background-color: white;
   font-family: "bitstream vera sans", "verdana", "sans";
   font-size: 14px;
}
a {
   color: #0000c0;
   text-decoration: none;
}
a.pkg {
   color: black;
}
a:hover {
   text-decoration: underline;
}
td.main {
   border-style: none;
}
blockquote {
   font-size: 12px;
}
td.package {
   background-color: #f0f0f0;
   vertical-align: top;
}
td.spacer {
   height: 5px;
}
td.version {
   background-color: #d0d0d0;
   vertical-align: top;
   text-align: left;
   padding: 5px;
   width: 100px;
}
p.manifest {
   font-size: 8px;
}
</style>
</head>
<body>
<h1>Available rocks</h1>
<p>
Lua modules available from this location for use with <a href="http://www.luarocks.org">LuaRocks</a>:
</p>
<table class="main">
`

	// indexPackageBegin opens the row of a rock (index_package_begin).
	indexPackageBegin = `<td class="package">
<p><a name="$anchor"></a><a href="#$anchor" class="pkg"><b>$package</b></a> - $summary<br/>
</p><blockquote><p>$detailed<br/>
$externaldependencies
<font size="-1"><a href="$original">latest sources</a> $homepage | License: $license</font></p>
</blockquote></a></td>
<td class="version">
`

	// indexPackageEnd closes the row of a rock (index_package_end).
	indexPackageEnd = `</td></tr>
<tr><td colspan="2" class="spacer"></td></tr>
`

	// indexFooterBegin closes the table and opens the footer (index_footer_begin).
	indexFooterBegin = `</table>
<p class="manifest">
<a href="manifest">manifest file</a>
`

	// indexManifestVer links the manifests of one Lua version (index_manifest_ver).
	indexManifestVer = `&bull; <a href="manifest-$VER">Lua $VER manifest file</a> (<a href="manifest-$VER.zip">zip</a>)
`

	// indexFooterEnd closes the page (index_footer_end).
	indexFooterEnd = `</p>
</body>
</html>
`
)
