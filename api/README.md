# house API

## Introduction
This API exposes a gRPC service contract to allow control of a house.

This API has drawn inspiration from several similar APIs, including:
- the [deconz REST API](https://dresden-elektronik.github.io/deconz-rest-doc/)
- the [Google HomeGraph API](https://developers.home.google.com/cloud-to-cloud/guides)
- the [Home Assistant API](https://developers.home-assistant.io/docs/api/rest)

However, the API designed here ultimately differs in both subtle and significant ways from these APIs. Given the domain space there is a lot of overlap in terminology; however it should not be assumed that since a given term is re-used it will mean exactly the same thing.

## API Structure
The purpose of this API is to interface with home automation systems in a consistent, technology-independent way. This is done by defining a few foundational primitives and exposing ways to interact with these primitives. These are:
- a bridge
- one or more devices controlled by the bridge

The bridge is usually a physical entity which connects to a computer or network, and acts as a gateway to the device or the home automation network which the devices connect to.

The bridge and device contracts both have distinct 'config' and 'state' elements. The 'config' element contains parameters which can be used to manipulate metadata associated with the parent, while the 'state' element contains parameters which manipulate the state of the parent itself. The intent of each object is that it is self-describing; it is possible for any consumer of the contract to understand what can be done to the device by reading properties directly from the element which describe the possible values of the different state elements.

There exist a number of implementations of this contract today included here. This package includes the contract definition, along with a couple of helper service definitions which can be included by particular implementations as needed.

The bridge contract is designed to support both major protocol approaches:
1. those technologies and SDKs which expose a synchronous, request/response style interface.
2. those technologies and SDKs which expose an asynchronous, event style interface.

There are a few foundational principles which implementers of this contract should keep in mind.
1. performing a set action on an element should cause the resulting state value to be propagated to any subscribed clients via the 'Update' stream. Some protocols (such as ZWave, Deconz, etc.) cause this to happen automatically; while more low-level protocols (such as X10) may require that the implementation perform this manually. Handling of this is provided automatically by the SyncBridgeService.
2. consumers of the contract should be able to assume the device ID is the one true identity of a device; and should it migrate between bridges the device itself will not change. As a result, clients of the contract will not intrinsically link devices to bridges outside the active connection between the client and the bridge.

Bridges advertise themselves over SSDP, using the `falnet_nerves:bridge` type. Advertisements are sent every 30 seconds.

---
### Devices

The most fundamental level of abstraction is that of a `device`. A device represents a single piece of hardware that is connected to the system using an underlying protocol. A device exists to be interacted with in some fashion or form. There are a few ways a device may be interacted with:
- it may expose readable state (for example, the brightness level of a light)
- it may allow for some of its state to be edited (for example, turning a light on or off)
- it may expose some readable configuration parameters (for example, what IP address the device has)
- it may allow for some of its configuration to be edited (for example, what time zone the device is in)
- it may have some readable, immutable properties (for example, the manufacturer name and model ID of the device)

These interactions are designed to be discoverable based upon the properties set - as an example if a light bulb is able to be dimmed it will also have a property set describing the minimum and maximum range of dimming supported.

### Bridges
A `device` does not exist in isolation. Every device is linked to a `bridge`. The bridge exists to describe how to communicate with the `device` - it is the entity translating the abstract changes made to a device into the appropriate, logical operations used on the physical channel required to make the change actually happen. It is possible for a bridge to have multiple devices; it is also possible for a bridge to have one or even no devices (if, for example, the bridge is currently out of range of any paired devices).

For now, the `bridge` is assumed to have a very technology-specific mechanism for linking `devices` in to the bridge and therefore no API for generically linking devices to a bridge exists - the implementor of each bridge will choose the appropriate mechanism for performing this operation.

Bridges can be discovered by listening for SSDP announcements with the `falnet_nerves:bridge` type.
