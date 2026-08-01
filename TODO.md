- could Client.Connect accept a nil io.ReadWriter if ConnectFunc were set?  ie, `if rw == nil && c.ConnectFunc != nil { rw, err = c.ConnectFunc(c.cfgModule) ... }`

- if Client is not our fs.FS, ClientConfig no longer holds weight as a separate struct (those should just be properties of the Client struct itself)
