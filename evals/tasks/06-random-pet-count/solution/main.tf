terraform {
  required_providers {
    random = { source = "hashicorp/random" }
  }
}

resource "random_pet" "servers" {
  count  = 3
  prefix = "web"
  length = 3
}
