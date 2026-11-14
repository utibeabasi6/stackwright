required_providers {
  random = {
    source  = "hashicorp/random"
    version = "3.7.2"
  }
}

variable "prefix" {
  type = string
}

provider "random" "this" {}

component "pet" {
  source = "./pet"
  inputs = {
    prefix = var.prefix
  }
  providers = {
    random = provider.random.this
  }
}

component "nulls" {
  source = "../shared/nulls"
  inputs = {
    pet = component.pet.name
  }
}
