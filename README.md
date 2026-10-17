# Namesplit

Windows DNS 分流客户端：按域名把查询转发到不同上游。

## 工作方式

Namesplit 在本机回环地址启动一个 DNS 解析器（默认 `127.0.0.1:53` / `::1:53`），由用户自行把需要生效的 DNS 指向该地址。应用不会修改任何系统设置。

- 按域名分流：精确 / `*.example.com` 通配 / 正则，命中后转发到指定上游组
- 本地记录：A / AAAA / CNAME / TXT / PTR，支持通配符与 TTL

## 状态

开发中，尚未发布。

## 许可

[Apache License 2.0](LICENSE)
