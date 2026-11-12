variable "name" {
  type = string
}

resource "terraform_data" "subnet" {
  input = var.name
}

module "sub" {
  source = "./sub"
  parent = terraform_data.subnet.id
}

output "subnet" {
  value = module.sub.id
}
