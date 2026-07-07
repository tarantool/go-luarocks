package = "maker"
version = "1.0-1"

source = {
   url = "https://example.com/maker-1.0.tar.gz",
}

build = {
   type = "make",
   build_target = "all",
   build_variables = {
      CC = "$(CC)",
      LIBFLAG = "$(LIBFLAG)",
   },
   install_target = "install",
   install_variables = {
      PREFIX = "$(PREFIX)",
   },
}
