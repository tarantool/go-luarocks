return {
   commands = {},
   modules = {
      ["metrics"] = {
         "metrics/1.0.0-1",
      },
      ["metrics.collectors.counter"] = {
         "metrics/1.0.0-1",
      },
   },
   repository = {
      metrics = {
         ["1.0.0-1"] = {
            {
               arch = "installed",
               commands = {},
               modules = {
                  metrics = "lua/metrics/init.lua",
                  ["metrics.collectors.counter"] = "lua/metrics/collectors/counter.lua",
               },
            },
         },
      },
   },
   dependencies = {
      metrics = {
         ["1.0.0-1"] = {
            {
               name = "checks",
               constraints = {
                  {
                     op = ">=",
                     version = { 3, 1, 0, string = "3.1.0" },
                  },
               },
            },
         },
      },
   },
}
