package = "cmaker"
version = "2.0-1"

source = {
   url = "git+https://example.com/cmaker.git",
   tag = "v2.0",
}

build = {
   type = "cmake",
   variables = {
      CMAKE_BUILD_TYPE = "Release",
      TARANTOOL_INCDIR = "$(TARANTOOL_INCDIR)",
   },
}
