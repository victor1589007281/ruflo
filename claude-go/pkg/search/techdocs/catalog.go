package techdocs

// catalog.go —— 技术文档与工程社区的**索引目录**。
//
// 这一层只回答两个问题:
//  1. 用户问某个产品时, 应该把检索限定到哪些官方域名;
//  2. 面板要展示哪些"技术文档索引地址"。
//
// 它不直接发请求, 也不依赖 pkg/search —— 目录是纯数据, 由 providers 包
// 消费 (匹配域名后交给 Exa 做 site: 限定检索), 由 search 包消费 (写进
// providers.json 供面板展示)。反过来依赖会让 search 与 providers 成环。
//
// 收录口径: 只收官方文档站与一线工程团队/社区, 不收 CSDN/知乎/公众号这类
// 洗稿与不可运行片段比例高的内容农场。单个站点的索引地址失效时, 检索仍
// 由 Exa 的站内索引兜底, 不影响其他站。

// Group 是目录的一级分组 (面板按它折叠展示)。
type Group string

const (
	GroupDatabase   Group = "database"
	GroupCloud      Group = "cloud"
	GroupOS         Group = "os"
	GroupStorage    Group = "storage"
	GroupNetwork    Group = "network"
	GroupMiddleware Group = "middleware"
	GroupCommunity  Group = "community"
)

// GroupOrder 是面板里的分组顺序: 先基础软件, 再工程社区。
var GroupOrder = []Group{
	GroupDatabase, GroupCloud, GroupOS, GroupStorage,
	GroupNetwork, GroupMiddleware, GroupCommunity,
}

// GroupNames 是分组展示名。
var GroupNames = map[Group]string{
	GroupDatabase:   "专业数据库",
	GroupCloud:      "云厂商 / 云原生",
	GroupOS:         "操作系统",
	GroupStorage:    "存储",
	GroupNetwork:    "网络 / 网关 / 基础设施",
	GroupMiddleware: "中间件 / 协议",
	GroupCommunity:  "技术社区 / 工程博客",
}

// Site 是一个文档站/社区。
//
// Name 是展示名; Domains 是 site: 限定用的域名 (不含 scheme);
// Aliases 用于从 query 里识别"用户在问这个产品", 英文按词边界匹配,
// 中文按子串匹配。Aliases 里不要放 "es"/"os"/"ai" 这类两字母通用词 ——
// 它们会把普通查询误判成技术查询。
//
// Lang 是该站正文的主要语言 ("zh" / "en")。留空表示语言中立 (官方文档站
// 通常有多语言版本)。目前只有组兜底用它: 兜底是按目录顺序取前几个站, 不做
// 语言约束会把英文查询指到中文站 (见 MatchQueryLang)。
type Site struct {
	Name    string   `json:"name"`
	Group   Group    `json:"group"`
	Docs    string   `json:"url"`
	Domains []string `json:"domains"`
	Aliases []string `json:"-"`
	Lang    string   `json:"lang,omitempty"`
}

// Sites 是全部收录站点。组内顺序就是兜底时的优先级: 核心站放前面。
var Sites = []Site{
	// ── 专业数据库 ────────────────────────────────────────────────────────
	{Name: "PostgreSQL", Group: GroupDatabase, Docs: "https://www.postgresql.org/docs/", Domains: []string{"postgresql.org"},
		Aliases: []string{"postgresql", "postgres", "pgsql"}},
	{Name: "MySQL", Group: GroupDatabase, Docs: "https://dev.mysql.com/doc/", Domains: []string{"dev.mysql.com", "mysql.com"},
		Aliases: []string{"mysql", "innodb", "myisam"}},
	{Name: "MariaDB", Group: GroupDatabase, Docs: "https://mariadb.com/docs/", Domains: []string{"mariadb.com"},
		Aliases: []string{"mariadb", "maria db"}},
	{Name: "SQLite", Group: GroupDatabase, Docs: "https://www.sqlite.org/docs.html", Domains: []string{"sqlite.org"},
		Aliases: []string{"sqlite"}},
	{Name: "Oracle Database", Group: GroupDatabase, Docs: "https://docs.oracle.com/en/database/oracle/oracle-database/", Domains: []string{"docs.oracle.com"},
		Aliases: []string{"oracle database", "oracle db", "oracle 19c", "oracle 23c", "oracle rac", "exadata"}},
	{Name: "SQL Server", Group: GroupDatabase, Docs: "https://learn.microsoft.com/en-us/sql/sql-server/", Domains: []string{"learn.microsoft.com"},
		Aliases: []string{"sql server", "mssql", "t-sql", "tsql"}},
	{Name: "MongoDB", Group: GroupDatabase, Docs: "https://www.mongodb.com/docs/", Domains: []string{"mongodb.com"},
		Aliases: []string{"mongodb", "mongo", "wiredtiger"}},
	{Name: "Redis", Group: GroupDatabase, Docs: "https://redis.io/docs/latest/", Domains: []string{"redis.io"},
		Aliases: []string{"redis", "redis cluster", "redis sentinel"}},
	{Name: "Elasticsearch", Group: GroupDatabase, Docs: "https://www.elastic.co/docs/reference/elasticsearch", Domains: []string{"elastic.co"},
		Aliases: []string{"elasticsearch", "elastic search", "kibana", "logstash", "elastic stack"}},
	{Name: "ClickHouse", Group: GroupDatabase, Docs: "https://clickhouse.com/docs", Domains: []string{"clickhouse.com"},
		Aliases: []string{"clickhouse", "click house"}},
	{Name: "Apache Cassandra", Group: GroupDatabase, Docs: "https://cassandra.apache.org/doc/latest/", Domains: []string{"cassandra.apache.org"},
		Aliases: []string{"cassandra", "cql"}},
	{Name: "Neo4j", Group: GroupDatabase, Docs: "https://neo4j.com/docs/", Domains: []string{"neo4j.com"},
		Aliases: []string{"neo4j", "cypher"}},
	{Name: "TiDB", Group: GroupDatabase, Docs: "https://docs.pingcap.com/tidb/stable/", Domains: []string{"docs.pingcap.com"},
		Aliases: []string{"tidb", "tikv", "tiflash", "pingcap"}},
	{Name: "OceanBase", Group: GroupDatabase, Docs: "https://www.oceanbase.com/docs", Domains: []string{"oceanbase.com"},
		Aliases: []string{"oceanbase", "ocean base"}},
	{Name: "DuckDB", Group: GroupDatabase, Docs: "https://duckdb.org/docs/", Domains: []string{"duckdb.org"},
		Aliases: []string{"duckdb"}},
	{Name: "Apache Doris", Group: GroupDatabase, Docs: "https://doris.apache.org/docs/", Domains: []string{"doris.apache.org"},
		Aliases: []string{"apache doris", "doris"}},
	{Name: "StarRocks", Group: GroupDatabase, Docs: "https://docs.starrocks.io/", Domains: []string{"docs.starrocks.io", "starrocks.io"},
		Aliases: []string{"starrocks", "star rocks"}},
	{Name: "CockroachDB", Group: GroupDatabase, Docs: "https://docs.cockroachlabs.com/docs", Domains: []string{"cockroachlabs.com"},
		Aliases: []string{"cockroachdb", "cockroach db", "crdb"}},
	{Name: "InfluxDB", Group: GroupDatabase, Docs: "https://docs.influxdata.com/influxdb/", Domains: []string{"influxdata.com"},
		Aliases: []string{"influxdb", "influx db", "flux"}},
	{Name: "Milvus", Group: GroupDatabase, Docs: "https://milvus.io/docs", Domains: []string{"milvus.io"},
		Aliases: []string{"milvus"}},
	{Name: "Qdrant", Group: GroupDatabase, Docs: "https://qdrant.tech/documentation/", Domains: []string{"qdrant.tech"},
		Aliases: []string{"qdrant"}},
	{Name: "Weaviate", Group: GroupDatabase, Docs: "https://docs.weaviate.io/weaviate", Domains: []string{"weaviate.io"},
		Aliases: []string{"weaviate"}},
	{Name: "Apache HBase", Group: GroupDatabase, Docs: "https://hbase.apache.org/book.html", Domains: []string{"hbase.apache.org"},
		Aliases: []string{"hbase"}},
	{Name: "openGauss", Group: GroupDatabase, Docs: "https://docs.opengauss.org/zh/", Domains: []string{"opengauss.org"},
		Aliases: []string{"opengauss", "open gauss"}},
	{Name: "达梦 DM", Group: GroupDatabase, Docs: "https://eco.dameng.com/document/dm/zh-cn/pm/", Domains: []string{"dameng.com"},
		Aliases: []string{"达梦", "dameng", "dm8"}},
	{Name: "人大金仓 KingbaseES", Group: GroupDatabase, Docs: "https://help.kingbase.com.cn/", Domains: []string{"kingbase.com.cn"},
		Aliases: []string{"人大金仓", "kingbase", "kingbasees"}},
	{Name: "华为 GaussDB", Group: GroupDatabase, Docs: "https://support.huaweicloud.com/gaussdb/", Domains: []string{"support.huaweicloud.com"},
		Aliases: []string{"gaussdb", "gauss db", "华为 gaussdb"}},
	{Name: "阿里 PolarDB", Group: GroupDatabase, Docs: "https://help.aliyun.com/zh/polardb/", Domains: []string{"help.aliyun.com"},
		Aliases: []string{"polardb", "polar db", "阿里云 polardb"}},
	{Name: "腾讯 TDSQL", Group: GroupDatabase, Docs: "https://cloud.tencent.com/document/product/557", Domains: []string{"cloud.tencent.com"},
		Aliases: []string{"tdsql", "腾讯 tdsql"}},

	// ── 云厂商 / 云原生 ───────────────────────────────────────────────────
	{Name: "AWS", Group: GroupCloud, Docs: "https://docs.aws.amazon.com/", Domains: []string{"docs.aws.amazon.com", "aws.amazon.com"},
		Aliases: []string{"aws", "amazon web services", "ec2", "s3", "rds", "lambda", "dynamodb", "eks", "cloudfront"}},
	{Name: "Microsoft Azure", Group: GroupCloud, Docs: "https://learn.microsoft.com/azure/", Domains: []string{"learn.microsoft.com"},
		Aliases: []string{"azure", "microsoft azure", "aks", "azure sql", "cosmos db", "azure devops"}},
	{Name: "Google Cloud", Group: GroupCloud, Docs: "https://cloud.google.com/docs", Domains: []string{"cloud.google.com"},
		Aliases: []string{"gcp", "google cloud", "bigquery", "gke", "cloud run", "google kubernetes engine"}},
	{Name: "Alibaba Cloud (intl)", Group: GroupCloud, Docs: "https://www.alibabacloud.com/help/en", Domains: []string{"alibabacloud.com"},
		Aliases: []string{"alibaba cloud", "alibabacloud"}},
	{Name: "阿里云", Group: GroupCloud, Docs: "https://help.aliyun.com/zh", Domains: []string{"help.aliyun.com"},
		Aliases: []string{"阿里云", "aliyun", "阿里云 ecs", "阿里云 oss", "阿里云 rds"}},
	{Name: "腾讯云", Group: GroupCloud, Docs: "https://cloud.tencent.com/document/", Domains: []string{"cloud.tencent.com"},
		Aliases: []string{"腾讯云", "tencent cloud", "qcloud"}},
	{Name: "华为云", Group: GroupCloud, Docs: "https://support.huaweicloud.com/", Domains: []string{"support.huaweicloud.com"},
		Aliases: []string{"华为云", "huawei cloud"}},
	{Name: "Oracle Cloud", Group: GroupCloud, Docs: "https://docs.oracle.com/en/cloud/", Domains: []string{"docs.oracle.com"},
		Aliases: []string{"oracle cloud", "oci", "oracle cloud infrastructure"}},
	{Name: "IBM Cloud", Group: GroupCloud, Docs: "https://cloud.ibm.com/docs", Domains: []string{"cloud.ibm.com"},
		Aliases: []string{"ibm cloud", "ibm cloud kubernetes"}},
	{Name: "DigitalOcean", Group: GroupCloud, Docs: "https://docs.digitalocean.com/", Domains: []string{"docs.digitalocean.com"},
		Aliases: []string{"digitalocean", "digital ocean"}},
	{Name: "Cloudflare", Group: GroupCloud, Docs: "https://developers.cloudflare.com/", Domains: []string{"developers.cloudflare.com"},
		Aliases: []string{"cloudflare", "cloudflare workers", "cloudflare pages", "cloudflare r2"}},
	{Name: "Kubernetes", Group: GroupCloud, Docs: "https://kubernetes.io/docs/", Domains: []string{"kubernetes.io"},
		Aliases: []string{"kubernetes", "k8s", "kubectl", "kubelet", "kube-apiserver", "kube-proxy", "kube-scheduler"}},
	{Name: "Docker", Group: GroupCloud, Docs: "https://docs.docker.com/", Domains: []string{"docs.docker.com"},
		Aliases: []string{"docker", "docker compose", "dockerfile"}},
	{Name: "OpenShift", Group: GroupCloud, Docs: "https://docs.openshift.com/", Domains: []string{"docs.openshift.com"},
		Aliases: []string{"openshift", "red hat openshift", "ocp"}},

	// ── 操作系统 ─────────────────────────────────────────────────────────
	{Name: "Linux Kernel", Group: GroupOS, Docs: "https://docs.kernel.org/", Domains: []string{"docs.kernel.org"},
		Aliases: []string{"linux kernel", "kernel module", "syscall", "system call", "cgroup", "io_uring", "netfilter", "lsm"}},
	{Name: "RHEL", Group: GroupOS, Docs: "https://docs.redhat.com/en/documentation/red_hat_enterprise_linux/", Domains: []string{"docs.redhat.com"},
		Aliases: []string{"rhel", "red hat enterprise linux", "redhat", "red hat"}},
	{Name: "Ubuntu", Group: GroupOS, Docs: "https://documentation.ubuntu.com/", Domains: []string{"documentation.ubuntu.com", "ubuntu.com"},
		Aliases: []string{"ubuntu", "ubuntu server"}},
	{Name: "Debian", Group: GroupOS, Docs: "https://www.debian.org/doc/", Domains: []string{"debian.org"},
		Aliases: []string{"debian", "apt"}},
	{Name: "Rocky Linux", Group: GroupOS, Docs: "https://docs.rockylinux.org/", Domains: []string{"docs.rockylinux.org"},
		Aliases: []string{"rocky linux", "rockylinux"}},
	{Name: "AlmaLinux", Group: GroupOS, Docs: "https://wiki.almalinux.org/", Domains: []string{"wiki.almalinux.org"},
		Aliases: []string{"almalinux", "alma linux"}},
	{Name: "SUSE", Group: GroupOS, Docs: "https://documentation.suse.com/", Domains: []string{"documentation.suse.com"},
		Aliases: []string{"suse", "sles", "opensuse", "suse linux"}},
	{Name: "Arch Wiki", Group: GroupOS, Docs: "https://wiki.archlinux.org/title/Main_page", Domains: []string{"wiki.archlinux.org"},
		Aliases: []string{"archlinux", "arch linux", "arch wiki", "pacman"}},
	{Name: "FreeBSD", Group: GroupOS, Docs: "https://docs.freebsd.org/en/", Domains: []string{"docs.freebsd.org"},
		Aliases: []string{"freebsd", "free bsd", "freebsd jail"}},
	{Name: "Windows Server", Group: GroupOS, Docs: "https://learn.microsoft.com/en-us/windows-server/", Domains: []string{"learn.microsoft.com"},
		Aliases: []string{"windows server", "windows server 2022", "windows server 2025", "active directory", "hyper-v"}},
	{Name: "openEuler", Group: GroupOS, Docs: "https://docs.openeuler.openatom.cn/zh/", Domains: []string{"docs.openeuler.openatom.cn"},
		Aliases: []string{"openeuler", "open euler", "欧拉"}},
	{Name: "OpenAnolis", Group: GroupOS, Docs: "https://docs.openanolis.cn/", Domains: []string{"docs.openanolis.cn"},
		Aliases: []string{"anolis", "openanolis", "龙蜥"}},
	{Name: "统信 UOS", Group: GroupOS, Docs: "https://uosdn.uniontech.com/#document", Domains: []string{"uosdn.uniontech.com"},
		Aliases: []string{"统信", "统信 uos", "uniontech", "uos"}},
	{Name: "Apple Developer", Group: GroupOS, Docs: "https://developer.apple.com/documentation/", Domains: []string{"developer.apple.com"},
		Aliases: []string{"apple developer", "macos", "ios development", "xcode"}},

	// ── 存储 ─────────────────────────────────────────────────────────────
	{Name: "Ceph", Group: GroupStorage, Docs: "https://docs.ceph.com/en/latest/", Domains: []string{"docs.ceph.com"},
		Aliases: []string{"ceph", "ceph osd", "ceph rgw", "ceph fs", "rados"}},
	{Name: "MinIO", Group: GroupStorage, Docs: "https://docs.min.io/", Domains: []string{"docs.min.io", "min.io"},
		Aliases: []string{"minio", "minio s3"}},
	{Name: "GlusterFS", Group: GroupStorage, Docs: "https://docs.gluster.org/en/latest/", Domains: []string{"docs.gluster.org"},
		Aliases: []string{"glusterfs", "gluster"}},
	{Name: "Lustre", Group: GroupStorage, Docs: "https://www.lustre.org/documentation/", Domains: []string{"lustre.org"},
		Aliases: []string{"lustre", "lustre fs"}},
	{Name: "JuiceFS", Group: GroupStorage, Docs: "https://juicefs.com/docs/community/introduction/", Domains: []string{"juicefs.com"},
		Aliases: []string{"juicefs", "juice fs"}},
	{Name: "Apache Hadoop", Group: GroupStorage, Docs: "https://hadoop.apache.org/docs/stable/", Domains: []string{"hadoop.apache.org"},
		Aliases: []string{"hadoop", "hdfs", "hadoop distributed file system", "yarn", "mapreduce"}},
	{Name: "OpenZFS", Group: GroupStorage, Docs: "https://openzfs.github.io/openzfs-docs/", Domains: []string{"openzfs.github.io"},
		Aliases: []string{"openzfs", "zfs", "zpool"}},
	{Name: "Btrfs", Group: GroupStorage, Docs: "https://btrfs.readthedocs.io/", Domains: []string{"btrfs.readthedocs.io"},
		Aliases: []string{"btrfs", "b-tree file system"}},
	{Name: "Longhorn", Group: GroupStorage, Docs: "https://longhorn.io/docs/", Domains: []string{"longhorn.io"},
		Aliases: []string{"longhorn", "longhorn storage"}},
	{Name: "Rook", Group: GroupStorage, Docs: "https://rook.io/docs/rook/latest/", Domains: []string{"rook.io"},
		Aliases: []string{"rook", "rook ceph"}},
	{Name: "OpenEBS", Group: GroupStorage, Docs: "https://openebs.io/docs/", Domains: []string{"openebs.io"},
		Aliases: []string{"openebs", "open ebs"}},
	{Name: "Alluxio", Group: GroupStorage, Docs: "https://documentation.alluxio.io/", Domains: []string{"documentation.alluxio.io"},
		Aliases: []string{"alluxio"}},
	{Name: "SeaweedFS", Group: GroupStorage, Docs: "https://github.com/seaweedfs/seaweedfs/wiki", Domains: []string{"github.com/seaweedfs"},
		Aliases: []string{"seaweedfs", "seaweed fs"}},
	{Name: "Linux NFS", Group: GroupStorage, Docs: "https://linux-nfs.org/wiki/", Domains: []string{"linux-nfs.org"},
		Aliases: []string{"nfs", "nfsv4", "network file system", "nfs mount"}},

	// ── 网络 / 网关 / 基础设施 ────────────────────────────────────────────
	{Name: "Nginx", Group: GroupNetwork, Docs: "https://nginx.org/en/docs/", Domains: []string{"nginx.org"},
		Aliases: []string{"nginx", "ngx_http"}},
	{Name: "HAProxy", Group: GroupNetwork, Docs: "https://docs.haproxy.org/", Domains: []string{"docs.haproxy.org"},
		Aliases: []string{"haproxy", "haproxy config"}},
	{Name: "Envoy", Group: GroupNetwork, Docs: "https://www.envoyproxy.io/docs", Domains: []string{"envoyproxy.io"},
		Aliases: []string{"envoy", "envoy proxy", "envoy filter"}},
	{Name: "Istio", Group: GroupNetwork, Docs: "https://istio.io/latest/docs/", Domains: []string{"istio.io"},
		Aliases: []string{"istio", "istio service mesh", "virtualservice", "destinationrule"}},
	{Name: "Calico", Group: GroupNetwork, Docs: "https://docs.tigera.io/calico/latest/about", Domains: []string{"docs.tigera.io"},
		Aliases: []string{"calico", "calico network policy"}},
	{Name: "Cilium", Group: GroupNetwork, Docs: "https://docs.cilium.io/en/stable/", Domains: []string{"docs.cilium.io"},
		Aliases: []string{"cilium", "cilium network policy", "hubble"}},
	{Name: "Traefik", Group: GroupNetwork, Docs: "https://doc.traefik.io/traefik/", Domains: []string{"doc.traefik.io"},
		Aliases: []string{"traefik"}},
	{Name: "CoreDNS", Group: GroupNetwork, Docs: "https://coredns.io/manual/", Domains: []string{"coredns.io"},
		Aliases: []string{"coredns", "core dns"}},
	{Name: "BIND 9", Group: GroupNetwork, Docs: "https://bind9.readthedocs.io/", Domains: []string{"bind9.readthedocs.io"},
		Aliases: []string{"bind9", "bind 9", "named.conf"}},
	{Name: "FRRouting", Group: GroupNetwork, Docs: "https://docs.frrouting.org/", Domains: []string{"docs.frrouting.org"},
		Aliases: []string{"frr", "frrouting", "bgpd", "ospfd"}},
	{Name: "Open vSwitch", Group: GroupNetwork, Docs: "https://docs.openvswitch.org/", Domains: []string{"docs.openvswitch.org"},
		Aliases: []string{"openvswitch", "open vswitch", "ovs", "ovs-vsctl"}},
	{Name: "etcd", Group: GroupNetwork, Docs: "https://etcd.io/docs/", Domains: []string{"etcd.io"},
		Aliases: []string{"etcd", "etcd raft"}},
	{Name: "Consul", Group: GroupNetwork, Docs: "https://developer.hashicorp.com/consul/docs", Domains: []string{"developer.hashicorp.com"},
		Aliases: []string{"consul", "consul service discovery"}},
	{Name: "Vault", Group: GroupNetwork, Docs: "https://developer.hashicorp.com/vault/docs", Domains: []string{"developer.hashicorp.com"},
		Aliases: []string{"vault", "hashicorp vault"}},
	{Name: "Terraform", Group: GroupNetwork, Docs: "https://developer.hashicorp.com/terraform/docs", Domains: []string{"developer.hashicorp.com"},
		Aliases: []string{"terraform", "terraform provider"}},
	{Name: "Tailscale", Group: GroupNetwork, Docs: "https://tailscale.com/docs", Domains: []string{"tailscale.com"},
		Aliases: []string{"tailscale", "tailscale acl", "tailnet"}},
	{Name: "RFC Editor", Group: GroupNetwork, Docs: "https://www.rfc-editor.org/", Domains: []string{"rfc-editor.org"},
		Aliases: []string{"rfc", "ietf", "rfc editor"}},

	// ── 中间件 / 协议 ─────────────────────────────────────────────────────
	{Name: "Apache Kafka", Group: GroupMiddleware, Docs: "https://kafka.apache.org/documentation/", Domains: []string{"kafka.apache.org"},
		Aliases: []string{"kafka", "apache kafka", "kafka streams", "kafka connect", "kraft"}},
	{Name: "RabbitMQ", Group: GroupMiddleware, Docs: "https://www.rabbitmq.com/docs", Domains: []string{"rabbitmq.com"},
		Aliases: []string{"rabbitmq", "rabbit mq", "amqp"}},
	{Name: "Apache Pulsar", Group: GroupMiddleware, Docs: "https://pulsar.apache.org/docs/", Domains: []string{"pulsar.apache.org"},
		Aliases: []string{"pulsar", "apache pulsar"}},
	{Name: "Apache ZooKeeper", Group: GroupMiddleware, Docs: "https://zookeeper.apache.org/doc/current/", Domains: []string{"zookeeper.apache.org"},
		Aliases: []string{"zookeeper", "zookeeper ensemble"}},
	{Name: "gRPC", Group: GroupMiddleware, Docs: "https://grpc.io/docs/", Domains: []string{"grpc.io"},
		Aliases: []string{"grpc", "grpc-go"}},
	{Name: "Protocol Buffers", Group: GroupMiddleware, Docs: "https://protobuf.dev/", Domains: []string{"protobuf.dev"},
		Aliases: []string{"protobuf", "protocol buffers", "proto3"}},

	// ── 技术社区 / 工程博客 ───────────────────────────────────────────────
	{Name: "LWN.net", Group: GroupCommunity, Docs: "https://lwn.net/", Domains: []string{"lwn.net"},
		Aliases: []string{"lwn", "lwn.net"}},
	{Name: "Phoronix", Group: GroupCommunity, Docs: "https://www.phoronix.com/", Domains: []string{"phoronix.com"},
		Aliases: []string{"phoronix"}},
	{Name: "InfoQ 中文", Group: GroupCommunity, Lang: "zh", Docs: "https://www.infoq.cn/", Domains: []string{"infoq.cn"},
		Aliases: []string{"infoq", "infoq 中文"}},
	{Name: "掘金", Group: GroupCommunity, Lang: "zh", Docs: "https://juejin.cn/", Domains: []string{"juejin.cn"},
		Aliases: []string{"掘金", "juejin"}},
	{Name: "博客园", Group: GroupCommunity, Lang: "zh", Docs: "https://www.cnblogs.com/", Domains: []string{"cnblogs.com"},
		Aliases: []string{"博客园", "cnblogs"}},
	{Name: "美团技术团队", Group: GroupCommunity, Lang: "zh", Docs: "https://tech.meituan.com/", Domains: []string{"tech.meituan.com"},
		Aliases: []string{"美团技术", "美团技术团队", "meituan tech"}},
	{Name: "Netflix TechBlog", Group: GroupCommunity, Docs: "https://netflixtechblog.com/", Domains: []string{"netflixtechblog.com"},
		Aliases: []string{"netflix techblog", "netflix engineering"}},
	{Name: "AWS Blog", Group: GroupCommunity, Docs: "https://aws.amazon.com/blogs/", Domains: []string{"aws.amazon.com/blogs"},
		Aliases: []string{"aws blog", "aws architecture blog", "aws news blog"}},
	{Name: "Azure Blog", Group: GroupCommunity, Docs: "https://azure.microsoft.com/en-us/blog/", Domains: []string{"azure.microsoft.com"},
		Aliases: []string{"azure blog", "azure updates"}},
	{Name: "Cloudflare Blog", Group: GroupCommunity, Docs: "https://blog.cloudflare.com/", Domains: []string{"blog.cloudflare.com"},
		Aliases: []string{"cloudflare blog"}},
	{Name: "Percona Blog", Group: GroupCommunity, Docs: "https://www.percona.com/blog/", Domains: []string{"percona.com"},
		Aliases: []string{"percona", "percona blog", "percona toolkit"}},
	{Name: "PingCAP Blog", Group: GroupCommunity, Docs: "https://www.pingcap.com/blog/", Domains: []string{"pingcap.com"},
		Aliases: []string{"pingcap", "pingcap blog"}},
	{Name: "ClickHouse Blog", Group: GroupCommunity, Docs: "https://clickhouse.com/blog", Domains: []string{"clickhouse.com"},
		Aliases: []string{"clickhouse blog"}},
	{Name: "Planet PostgreSQL", Group: GroupCommunity, Docs: "https://planet.postgresql.org/", Domains: []string{"planet.postgresql.org"},
		Aliases: []string{"planet postgresql", "planet postgres"}},
	{Name: "Brendan Gregg", Group: GroupCommunity, Docs: "https://www.brendangregg.com/", Domains: []string{"brendangregg.com"},
		Aliases: []string{"brendan gregg", "brendangregg", "flame graph", "flamegraph"}},
	{Name: "Julia Evans", Group: GroupCommunity, Docs: "https://jvns.ca/", Domains: []string{"jvns.ca"},
		Aliases: []string{"julia evans", "jvns"}},
	{Name: "Dan Luu", Group: GroupCommunity, Docs: "https://danluu.com/", Domains: []string{"danluu.com"},
		Aliases: []string{"dan luu", "danluu"}},
	{Name: "ACM Queue", Group: GroupCommunity, Docs: "https://queue.acm.org/", Domains: []string{"queue.acm.org"},
		Aliases: []string{"acm queue"}},
	{Name: "USENIX ;login:", Group: GroupCommunity, Docs: "https://www.usenix.org/publications/login", Domains: []string{"usenix.org"},
		Aliases: []string{"usenix", "usenix login", ";login"}},
	{Name: "High Scalability", Group: GroupCommunity, Docs: "https://highscalability.com/", Domains: []string{"highscalability.com"},
		Aliases: []string{"high scalability", "highscalability"}},
}

// SitesOfGroup 返回某组站点 (保持目录顺序)。
func SitesOfGroup(g Group) []Site {
	out := make([]Site, 0, 8)
	for _, s := range Sites {
		if s.Group == g {
			out = append(out, s)
		}
	}
	return out
}
