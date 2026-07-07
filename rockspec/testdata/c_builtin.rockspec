package = "cbuiltin"
version = "0.3-2"

source = {
   url = "https://example.com/cbuiltin-0.3.tar.gz",
}

dependencies = {
   "lua >= 5.1",
}

external_dependencies = {
   TARANTOOL = {
      header = "tarantool/module.h",
   },
   FOO = {
      library = "foo",
   },
}

build = {
   type = "builtin",
   modules = {
      ["cbuiltin.core"] = {
         sources   = { "src/core.c", "src/aux.c" },
         incdirs   = { "$(TARANTOOL_INCDIR)" },
         libdirs   = { "$(FOO_LIBDIR)" },
         libraries = { "foo" },
         defines   = { "CBUILTIN_DEBUG=1", "_GNU_SOURCE" },
      },
   },
}
