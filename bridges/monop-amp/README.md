# monop-amp

This bridge interfaces with a Monoprice stero amplifier.

At the moment, this bridge queries the state of the amplifier at startup and then doesn't refresh it. I have seen issues with the amp not properly sending updates over the serial bus, so it's easiest to avoid the potential for corrupt data and just to execute the received commands.