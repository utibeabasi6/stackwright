terraform {
  required_providers {
    random = { source = "hashicorp/random" }
  }
}

resource "random_password" "db" {
  length  = 24
  special = false
}
