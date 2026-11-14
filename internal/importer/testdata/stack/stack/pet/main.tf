variable "prefix" {}

resource "random_pet" "this" {
  prefix = var.prefix
}

output "name" {
  value = random_pet.this.id
}
