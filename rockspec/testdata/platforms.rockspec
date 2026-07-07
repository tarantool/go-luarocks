package = "platformer"
version = "1.0-1"

source = {
   url = "https://example.com/platformer-1.0.tar.gz",
}

build = {
   type = "builtin",
   modules = {
      ["platformer"] = "src/platformer.lua",
   },
   platforms = {
      unix = {
         modules = {
            ["platformer.posix"] = "src/posix.lua",
         },
      },
      linux = {
         modules = {
            ["platformer.linux"] = "src/linux.lua",
         },
      },
      macosx = {
         modules = {
            ["platformer.macosx"] = "src/macosx.lua",
         },
      },
   },
}
