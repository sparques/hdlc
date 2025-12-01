# HDLC

This package does not implement any standardized form of HDLC (High-Level Data Link Control), but is a general purpose implementation of an HDLC-like protocol that exposes an idiomatic-Go interface.

This wraps an io.ReadWriter and presents an io.ReadWriter, and thus should work with anything that uses io.Read and io.Write interfaces. The CRC check and retry happen transparently.

# Purpose

I wrote this specifically for use with an unreliable serial connection between a host PC and an RP2040-based MCU board running TinyGo firmware. 

It should, however, be suitable for most unreliable duplex (or hardware flow-controled simplex) communication pipes. 

# Example

```
u := openYourUART() // implements io.ReadWriter

f := hdlc.NewFramer(u,
    hdlc.WithRetryInterval(250*time.Millisecond),
    hdlc.WithMaxRetries(8),
)

// Default is autoFlush=true; each Write is its own HDLC payload.
go func() {
    // writer side
    _, err := f.Write([]byte("hello over unreliable UART"))
    if err != nil {
        // handle
    }
}()

buf := make([]byte, 1024)
n, err := f.Read(buf)
if err != nil {
    // handle
}
println(string(buf[:n]))

``` 