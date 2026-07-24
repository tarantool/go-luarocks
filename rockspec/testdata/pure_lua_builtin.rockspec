package = "purelua"
version = "1.2-1"
rockspec_format = "3.0"

source = {
   url = "https://example.com/purelua-1.2.tar.gz",
   md5 = "0123456789abcdef0123456789abcdef",
   dir = "purelua-1.2",
}

description = {
   summary = "Pure-Lua test rock",
   detailed = "Multi-line\ndescription.",
   license = "MIT",
   homepage = "https://example.com/purelua",
   issues_url = "https://example.com/purelua/issues",
   maintainer = "test <test@example.com>",
   labels = { "test", "pure-lua" },
}

dependencies = {
   "lua >= 5.1",
   "checks >= 3.0, < 4",
}

supported_platforms = { "unix", "macosx" }

build = {
   type = "builtin",
   modules = {
      ["purelua"]       = "src/purelua.lua",
      ["purelua.inner"] = "src/purelua/inner.lua",
   },
   copy_directories = { "doc", "tests" },
   install = {
      lua = {
         ["purelua.extras"] = "extras/extras.lua",
      },
      bin = {
         ["purelua-cli"] = "bin/purelua-cli",
      },
   },
}
