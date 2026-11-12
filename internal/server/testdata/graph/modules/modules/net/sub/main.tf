variable "parent" {
  type = string
}

resource "terraform_data" "leaf" {
  input = var.parent
}

output "id" {
  value = terraform_data.leaf.id
}
