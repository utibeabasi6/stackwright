variable "name" {
  type = string
}

variable "vpc_id" {
  type = string
}

variable "port" {
  type    = number
  default = 8080
}

variable "health_check" {
  type = object({
    path             = string
    interval_seconds = number
  })
  default = {
    path             = "/"
    interval_seconds = 30
  }
  description = "Where and how often the load balancer checks the service."
}
