variable "name" {
  description = "Cluster/name prefix."
  type        = string
  default     = "loopsandbox"
}

variable "region" {
  description = "AWS region."
  type        = string
  default     = "us-east-1"
}

variable "tags" {
  description = "Common resource tags."
  type        = map(string)
  default     = { "app" = "loopsandbox" }
}

variable "vpc_cidr" {
  description = "VPC CIDR."
  type        = string
  default     = "10.20.0.0/16"
}

variable "private_subnet_cidrs" {
  description = "Private subnet CIDRs (one per AZ)."
  type        = list(string)
  default     = ["10.20.1.0/24", "10.20.2.0/24"]
}

variable "availability_zones" {
  description = "AZs matching the private subnets."
  type        = list(string)
  default     = ["us-east-1a", "us-east-1b"]
}

variable "kubernetes_version" {
  description = "EKS Kubernetes version."
  type        = string
  default     = "1.30"
}

variable "metal_instance_type" {
  description = "Bare-metal instance type for Firecracker workers (KVM/nested virt)."
  type        = string
  default     = "c5.metal"
}

variable "metal_desired_size" {
  type    = number
  default = 2
}

variable "metal_min_size" {
  type    = number
  default = 1
}

variable "metal_max_size" {
  type    = number
  default = 10
}

variable "postgres_version" {
  type    = string
  default = "16"
}

variable "db_instance_class" {
  type    = string
  default = "db.r6g.large"
}

variable "db_allocated_storage" {
  type    = number
  default = 100
}

variable "db_username" {
  type    = string
  default = "loopsandbox"
}

variable "db_password" {
  description = "RDS master password — supply via a secret manager reference, never a literal in VCS."
  type        = string
  sensitive   = true
}

output "cluster_endpoint" {
  value = aws_eks_cluster.this.endpoint
}

output "rds_endpoint" {
  value = aws_db_instance.postgres.address
}
