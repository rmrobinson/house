# bridge

The `bridge` package supports the development of services by abstracting away the management of incoming client connections & commands.

Within this package are:
- the `Service` type which exposes methods that are used by the bridge implementations to manage the state of the system; including a cache of the device states.
- the `API` type which conforms to the gRPC API definition for the bridge service. This speaks to the Service to retrieve state data or to request updates be made.

The typical implementation of a bridge would entail the following steps:
- defining a type which actually speaks the third party protocol, and conforms to the `Handler` interface. This type must be able to process the supplied commands; and to monitor the state of the devices and ensure these updates are shared back to the `Service` type. The `Service` maintains a cache of the device state, and will perform deduplication if necessary to ensure only actual changes are shared to the clients subscribed to the `API` stream.

Since a bridge might be started without having established communication with the remote system, the `Service` type is created first. When the `Handler` is ready to process requests, it should register itself with the `Service` using the `RegisterHandler` method. This signals to the remote clients that requests will be processed. After calling `RegisterHandler` the bridge should register the available devices through the `UpdateDevice` method on the service; and use both this and the `UpdateBridge` methods as further updates happen to ensure the state is kept in sync between the remote nodes and the `Service`.

Internally, the `Service` clones any object it receives from the handler to avoid changes from being made to the object without a related `Update` call being made.

## What Might Change?
- the API type is exported to allow bridge implementations to register the server itself - this might not actually end up being useful and could be made private
- the Source and Sink types should probably be moved to be either package private or refactored to be a separate library