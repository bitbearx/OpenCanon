// Package web 提供无需外部构建的网页资源。
package web

import "embed"

// FS 将手机网页随服务一起打包。
//go:embed index.html app.js style.css
var FS embed.FS
