module github.com/alist-org/alist/v3

go 1.25.0

// The plugin SDK boundary lives in an independent module under ./ext (v0.x,
// UNSTABLE until GA). It is developed in-tree via this replace directive; the
// published module path is github.com/AlistGo/alist-ext.
replace github.com/AlistGo/alist-ext => ./ext

// The remote agent (protocol + runtime) lives in an independent module under
// ./agent (v0.x, UNSTABLE). It is developed in-tree via this replace
// directive; the published module path is github.com/AlistGo/alist-agent.
replace github.com/AlistGo/alist-agent => ./agent

require (
	github.com/Xhofe/go-cache v0.0.0-20240804043513-b1a71927bc21
	github.com/AlistGo/alist-agent v0.1.0
	github.com/AlistGo/alist-ext v0.1.0
	github.com/alist-org/times v0.0.0-20240721124654-efa0c7d3ad92
	github.com/aws/aws-sdk-go v1.55.5
	github.com/blevesearch/bleve/v2 v2.4.2
	github.com/caarlos0/env/v9 v9.0.0
	github.com/charmbracelet/bubbles v0.20.0
	github.com/charmbracelet/bubbletea v1.1.0
	github.com/charmbracelet/lipgloss v0.13.0
	github.com/coreos/go-oidc/v3 v3.20.0
	github.com/deckarep/golang-set/v2 v2.6.0
	github.com/descope/virtualwebauthn v1.0.5
	github.com/disintegration/imaging v1.6.2
	github.com/dlclark/regexp2 v1.11.4
	github.com/gin-contrib/cors v1.7.2
	github.com/gin-gonic/gin v1.10.0
	github.com/go-gormigrate/gormigrate/v2 v2.1.4
	github.com/go-jose/go-jose/v4 v4.1.4
	github.com/go-resty/resty/v2 v2.14.0
	github.com/go-webauthn/webauthn v0.17.4
	github.com/golang-jwt/jwt/v4 v4.5.2
	github.com/google/uuid v1.6.0
	github.com/gorilla/websocket v1.5.3
	github.com/hashicorp/go-hclog v0.14.1
	github.com/hashicorp/go-plugin v1.6.3
	github.com/hekmon/cunits/v2 v2.1.0
	github.com/hekmon/transmissionrpc/v3 v3.0.0
	github.com/json-iterator/go v1.1.12
	github.com/maruel/natural v1.1.1
	github.com/meilisearch/meilisearch-go v0.27.2
	github.com/natefinch/lumberjack v2.0.0+incompatible
	github.com/pkg/errors v0.9.1
	github.com/pquerna/otp v1.4.0
	github.com/rclone/rclone v1.67.0
	github.com/sirupsen/logrus v1.9.3
	github.com/spf13/cobra v1.8.1
	github.com/stretchr/testify v1.11.1
	github.com/swaggo/swag v1.16.6
	github.com/u2takey/ffmpeg-go v0.5.0
	github.com/xhofe/tache v0.1.5
	golang.org/x/crypto v0.52.0
	golang.org/x/exp v0.0.0-20240904232852-e7e105dedf7e
	golang.org/x/image v0.19.0
	golang.org/x/net v0.54.0
	golang.org/x/oauth2 v0.36.0
	golang.org/x/time v0.8.0
	google.golang.org/grpc v1.79.3
	gorm.io/driver/mysql v1.5.7
	gorm.io/driver/postgres v1.5.9
	gorm.io/driver/sqlite v1.5.6
	gorm.io/gorm v1.25.12
)

require (
	cloud.google.com/go/compute/metadata v0.9.0 // indirect
	github.com/KyleBanks/depth v1.2.1 // indirect
	github.com/Max-Sum/base32768 v0.0.0-20230304063302-18e6ce5945fd // indirect
	github.com/PuerkitoBio/purell v1.1.1 // indirect
	github.com/PuerkitoBio/urlesc v0.0.0-20170810143723-de5bf2ad4578 // indirect
	github.com/RoaringBitmap/roaring v1.9.3 // indirect
	github.com/abbot/go-http-auth v0.4.0 // indirect
	github.com/andybalholm/brotli v1.1.1 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/bits-and-blooms/bitset v1.12.0 // indirect
	github.com/blevesearch/bleve_index_api v1.1.10 // indirect
	github.com/blevesearch/geo v0.1.20 // indirect
	github.com/blevesearch/go-faiss v1.0.20 // indirect
	github.com/blevesearch/go-porterstemmer v1.0.3 // indirect
	github.com/blevesearch/gtreap v0.1.1 // indirect
	github.com/blevesearch/mmap-go v1.0.4 // indirect
	github.com/blevesearch/scorch_segment_api/v2 v2.2.15 // indirect
	github.com/blevesearch/segment v0.9.1 // indirect
	github.com/blevesearch/snowballstem v0.9.0 // indirect
	github.com/blevesearch/upsidedown_store_api v1.0.2 // indirect
	github.com/blevesearch/vellum v1.0.10 // indirect
	github.com/blevesearch/zapx/v11 v11.3.10 // indirect
	github.com/blevesearch/zapx/v12 v12.3.10 // indirect
	github.com/blevesearch/zapx/v13 v13.3.10 // indirect
	github.com/blevesearch/zapx/v14 v14.3.10 // indirect
	github.com/blevesearch/zapx/v15 v15.3.13 // indirect
	github.com/blevesearch/zapx/v16 v16.1.5 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/coreos/go-semver v0.3.1 // indirect
	github.com/fatih/color v1.7.0 // indirect
	github.com/fxamacker/cbor/v2 v2.9.2 // indirect
	github.com/go-chi/chi/v5 v5.0.12 // indirect
	github.com/go-ole/go-ole v1.3.0 // indirect
	github.com/go-openapi/jsonpointer v0.19.5 // indirect
	github.com/go-openapi/jsonreference v0.19.6 // indirect
	github.com/go-openapi/spec v0.20.4 // indirect
	github.com/go-openapi/swag v0.19.15 // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/go-webauthn/x v0.2.6 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/golang/geo v0.0.0-20210211234256-740aa86cb551 // indirect
	github.com/golang/protobuf v1.5.4 // indirect
	github.com/golang/snappy v0.0.1 // indirect
	github.com/google/go-tpm v0.9.8 // indirect
	github.com/hashicorp/go-cleanhttp v0.5.2 // indirect
	github.com/hashicorp/yamux v0.1.1 // indirect
	github.com/josharian/intern v1.0.0 // indirect
	github.com/jzelinskie/whirlpool v0.0.0-20201016144138-0675e54bb004 // indirect
	github.com/klauspost/compress v1.17.8 // indirect
	github.com/lufia/plan9stats v0.0.0-20231016141302-07b5767bb0ed // indirect
	github.com/mailru/easyjson v0.7.7 // indirect
	github.com/mattn/go-colorable v0.1.13 // indirect
	github.com/mschoch/smat v0.2.0 // indirect
	github.com/oklog/run v1.0.0 // indirect
	github.com/philhofer/fwd v1.2.0 // indirect
	github.com/power-devops/perfstat v0.0.0-20221212215047-62379fc7944b // indirect
	github.com/prometheus/client_golang v1.19.1 // indirect
	github.com/prometheus/client_model v0.5.0 // indirect
	github.com/prometheus/common v0.48.0 // indirect
	github.com/prometheus/procfs v0.12.0 // indirect
	github.com/rfjakob/eme v1.1.2 // indirect
	github.com/rogpeppe/go-internal v1.14.1 // indirect
	github.com/shirou/gopsutil/v3 v3.24.4 // indirect
	github.com/shoenig/go-m1cpu v0.1.6 // indirect
	github.com/tinylib/msgp v1.6.4 // indirect
	github.com/tklauser/go-sysconf v0.3.13 // indirect
	github.com/tklauser/numcpus v0.7.0 // indirect
	github.com/valyala/bytebufferpool v1.0.0 // indirect
	github.com/valyala/fasthttp v1.37.1-0.20220607072126-8a320890c08d // indirect
	github.com/x448/float16 v0.8.4 // indirect
	github.com/yusufpapurcu/wmi v1.2.4 // indirect
	go.etcd.io/bbolt v1.3.8 // indirect
	golang.org/x/mod v0.35.0 // indirect
	golang.org/x/term v0.43.0 // indirect
	golang.org/x/tools v0.44.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20251202230838-ff82c1b0f217 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
)

require (
	github.com/aymerick/douceur v0.2.0 // indirect
	github.com/bytedance/sonic/loader v0.1.1 // indirect
	github.com/charmbracelet/x/ansi v0.2.3 // indirect
	github.com/charmbracelet/x/term v0.2.0 // indirect
	github.com/cloudwego/base64x v0.1.4 // indirect
	github.com/cloudwego/iasm v0.2.0 // indirect
	github.com/erikgeiser/coninput v0.0.0-20211004153227-1c3628e74d0f // indirect
	github.com/gorilla/css v1.0.1 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/matoous/go-nanoid/v2 v2.1.0 // indirect
	github.com/microcosm-cc/bluemonday v1.0.27
	github.com/yuin/goldmark v1.7.8
	go4.org v0.0.0-20230225012048-214862532bf5
)

require (
	github.com/aymanbagabas/go-osc52/v2 v2.0.1 // indirect
	github.com/boombuler/barcode v1.0.1-0.20190219062509-6c824513bacc // indirect
	github.com/bytedance/sonic v1.11.6 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/gabriel-vasile/mimetype v1.4.3 // indirect
	github.com/gin-contrib/sse v0.1.0 // indirect
	github.com/go-playground/locales v0.14.1 // indirect
	github.com/go-playground/universal-translator v0.18.1 // indirect
	github.com/go-playground/validator/v10 v10.20.0 // indirect
	github.com/go-sql-driver/mysql v1.7.0 // indirect
	github.com/goccy/go-json v0.10.2 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/pgx/v5 v5.9.0 // indirect
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/jinzhu/now v1.1.5 // indirect
	github.com/jmespath/go-jmespath v0.4.0 // indirect
	github.com/klauspost/cpuid/v2 v2.2.7 // indirect
	github.com/leodido/go-urn v1.4.0 // indirect
	github.com/lucasb-eyer/go-colorful v1.2.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mattn/go-localereader v0.0.1 // indirect
	github.com/mattn/go-runewidth v0.0.16 // indirect
	github.com/mattn/go-sqlite3 v1.14.22 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.2 // indirect
	github.com/muesli/ansi v0.0.0-20230316100256-276c6243b2f6 // indirect
	github.com/muesli/cancelreader v0.2.2 // indirect
	github.com/muesli/termenv v0.15.2 // indirect
	github.com/otiai10/copy v1.14.0
	github.com/pelletier/go-toml/v2 v2.2.2
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/spf13/pflag v1.0.5 // indirect
	github.com/twitchyliquid64/golang-asm v0.15.1 // indirect
	github.com/u2takey/go-utils v0.3.1 // indirect
	github.com/ugorji/go/codec v1.2.12 // indirect
	github.com/xhofe/gsync v0.0.0-20230917091818-2111ceb38a25 // indirect
	golang.org/x/arch v0.8.0 // indirect
	golang.org/x/sync v0.20.0
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	google.golang.org/protobuf v1.36.10 // indirect
	gopkg.in/natefinch/lumberjack.v2 v2.0.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace github.com/ProtonMail/go-proton-api => github.com/henrybear327/go-proton-api v1.0.0

replace github.com/cronokirby/saferith => github.com/Da3zKi7/saferith v0.33.0-fixed

replace github.com/SheltonZhu/115driver => github.com/okatu-loli/115driver v1.2.3-1
