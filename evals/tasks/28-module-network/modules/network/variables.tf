variable "name_prefix" {
  type        = string
  description = "Goes before the name of each resource."
}

variable "vpc_cidr_block" {
  type = string
}

variable "public_subnets" {
  type = map(object({
    cidr = string
    az   = string
  }))
  description = "Public subnets by name."
}

variable "enable_dns" {
  type    = bool
  default = true
}
